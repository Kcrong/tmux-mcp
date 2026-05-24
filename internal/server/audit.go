package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// AuditStderr is the magic value for -audit-log that routes audit records
// to stderr alongside the regular slog stream. Any other value is treated
// as a filesystem path opened in append-only mode (0600).
const AuditStderr = "stderr"

// Audit emits one JSONL record per `tools/call` to either a file or
// stderr. The schema is documented in [Audit.Record]: it deliberately
// excludes argument *content* (which can carry shell commands or
// secrets) and keeps only the byte length, so logs are safe to ship to
// long-term storage without leaking sensitive payloads.
//
// All methods are safe to call on a nil *Audit (they are no-ops),
// which keeps audit-disabled callers free of branchy guards. A non-nil
// Audit serialises every Record write through an internal mutex so
// concurrent goroutines never interleave half-written lines.
type Audit struct {
	mu sync.Mutex
	// w is the destination writer. For file targets w is a buffered
	// writer wrapping the underlying *os.File so per-record write()
	// syscalls don't dominate the dispatcher's hot path.
	w io.Writer
	// flusher is the buffered-writer wrapper (set for files) so Close
	// can flush before closing the underlying fd. It's nil when w is
	// the unbuffered stderr writer.
	flusher *bufio.Writer
	// closer is the underlying *os.File for file targets, nil for
	// stderr (we don't own stderr's lifetime).
	closer io.Closer
}

// OpenAudit returns an [Audit] writing to path. The two special cases:
//
//   - path == "" — audit is disabled; returns (nil, nil) so callers can
//     pass the result straight through without nil checks.
//   - path == [AuditStderr] — records go to the supplied stderr writer
//     (no file is opened, no closer is held).
//
// Any other value is treated as a filesystem path opened with
// O_APPEND|O_CREATE|O_WRONLY at mode 0600. On open failure the error
// is wrapped with the path so operators can immediately see what went
// wrong instead of guessing from a bare "permission denied".
func OpenAudit(path string, stderr io.Writer) (*Audit, error) {
	if path == "" {
		return nil, nil
	}
	if path == AuditStderr {
		// Stderr is shared with slog; no buffering or close needed.
		return &Audit{w: stderr}, nil
	}
	// O_APPEND keeps concurrent processes from clobbering each other's
	// records; mode 0600 keeps the audit log private to the operator
	// because it can carry hostnames, session names, and timing data.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log %q: %w", path, err)
	}
	bw := bufio.NewWriter(f)
	return &Audit{w: bw, flusher: bw, closer: f}, nil
}

// Close flushes any buffered records and closes the underlying file.
// Safe to call on nil. Stderr-backed audits have no closer so Close is
// a no-op there too.
func (a *Audit) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var flushErr, closeErr error
	if a.flusher != nil {
		flushErr = a.flusher.Flush()
	}
	if a.closer != nil {
		closeErr = a.closer.Close()
	}
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

// auditRecord is the JSONL schema written to the audit sink. Field
// order is fixed by the struct tags so records sort cleanly in tools
// like jq/grep that key on the leading field. ErrorCode is omitempty
// because it should only appear on failure — a successful call carries
// no JSON-RPC error code.
type auditRecord struct {
	Ts            string `json:"ts"`
	RequestID     string `json:"request_id"`
	Tool          string `json:"tool"`
	Session       any    `json:"session"`
	DurationMs    int64  `json:"duration_ms"`
	Result        string `json:"result"`
	ErrorCode     *int   `json:"error_code,omitempty"`
	ArgsSizeBytes int    `json:"args_size_bytes"`
}

// Record emits one JSONL line describing a `tools/call` invocation.
// It is deliberately strict about its inputs:
//
//   - ts is RFC3339 nano in UTC so timestamps sort lexicographically
//     and don't drift with the operator's local zone.
//   - session is best-effort: extracted from a "session" key in args
//     when present (string), null otherwise. We never deserialize the
//     full args struct — different tools have different shapes, and
//     scanning for one well-known key keeps Record cheap and decoupled
//     from tool definitions.
//   - args carries the *raw bytes* the dispatcher passed to the
//     handler. Only its length is recorded — args content can include
//     shell commands or credentials and must never be persisted.
//
// rerr nil means the handler succeeded; non-nil populates result="error"
// and the JSON-RPC error code.
//
// Record is a no-op when called on a nil *Audit, so the dispatcher can
// invoke it unconditionally without a branch.
func (a *Audit) Record(rid, tool string, args json.RawMessage, dur time.Duration, rerr *rpcError) {
	if a == nil {
		return
	}
	rec := auditRecord{
		Ts:            time.Now().UTC().Format(time.RFC3339Nano),
		RequestID:     rid,
		Tool:          tool,
		Session:       extractSession(args),
		DurationMs:    dur.Milliseconds(),
		Result:        "ok",
		ArgsSizeBytes: len(args),
	}
	if rerr != nil {
		rec.Result = "error"
		code := rerr.Code
		rec.ErrorCode = &code
	}
	line, err := json.Marshal(rec)
	if err != nil {
		// json.Marshal can only fail here on a programmer error
		// (an un-marshalable type slipped into auditRecord). Log to
		// the regular slog stream instead of dropping silently so the
		// gap is at least diagnosable.
		slog.Error("audit marshal failed", "err", err, "rid", rid, "tool", tool)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.w.Write(line); err != nil {
		slog.Error("audit write failed", "err", err, "rid", rid, "tool", tool)
		return
	}
	if _, err := a.w.Write([]byte{'\n'}); err != nil {
		slog.Error("audit write failed", "err", err, "rid", rid, "tool", tool)
	}
}

// extractSession peeks at args looking for a top-level "session"
// string. It deliberately ignores other shapes (numbers, objects,
// arrays) — the schema for every session-bearing tool today is a flat
// `{"session":"..."}` so anything else is a different tool entirely.
// Returns nil when the key is missing or not a string so the encoded
// JSON shows `"session": null`, signalling "not applicable" rather
// than empty-string "absent".
func extractSession(args json.RawMessage) any {
	if len(args) == 0 {
		return nil
	}
	// We only need the one field — decoding into a tiny struct is
	// cheaper than json.Unmarshal into map[string]any and skips the
	// allocation churn for every other field.
	var probe struct {
		Session *string `json:"session"`
	}
	if err := json.Unmarshal(args, &probe); err != nil {
		return nil
	}
	if probe.Session == nil {
		return nil
	}
	return *probe.Session
}

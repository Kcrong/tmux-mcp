package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAudit_NilHandleIsNoop pins the contract that callers can pass a
// nil *Audit unconditionally. Record / Close must succeed without
// panicking and without touching the (non-existent) writer. This is
// what keeps the dispatcher hot path branch-free in audit-disabled
// deployments.
func TestAudit_NilHandleIsNoop(t *testing.T) {
	t.Parallel()
	var a *Audit
	// Should not panic on any of these.
	a.Record("rid-1", "send_keys", json.RawMessage(`{"session":"x"}`), 5*time.Millisecond, nil)
	a.Record("rid-2", "send_keys", nil, 0, &rpcError{Code: -32602, Message: "bad"})
	if err := a.Close(); err != nil {
		t.Fatalf("Close on nil audit returned %v", err)
	}
}

// audit returns a *Audit that writes into the supplied buffer with the
// same locking discipline the production code uses. It's the test-only
// constructor so the file path / stderr cases are exercised separately
// (TestAudit_OpenAuditFile) without re-implementing the bookkeeping.
func newBufferAudit(buf io.Writer) *Audit {
	return &Audit{w: buf}
}

// readAuditLines splits the accumulated buffer on '\n' and decodes each
// non-empty line as a generic map. The helper centralises the JSONL
// parsing so individual tests assert on field shape, not framing.
func readAuditLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(buf.Bytes(), []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("decode audit line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// TestAudit_RecordsOneLinePerToolCall drives Serve with a tools/call
// frame and asserts exactly one audit record lands in the buffer with
// the expected schema (ts/request_id/tool/session/duration_ms/result/
// args_size_bytes). It also confirms non-tools/call methods (initialize,
// tools/list) are NOT audited — they're protocol bookkeeping.
func TestAudit_RecordsOneLinePerToolCall(t *testing.T) {
	t.Parallel()
	auditBuf := &bytes.Buffer{}
	audit := newBufferAudit(auditBuf)

	in := &threadSafeBuffer{}
	rpcOut := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: rpcOut, mu: outMu}

	// The handler accepts every tools/call by returning a tiny success
	// payload, so we drive the audit-success path. initialize and
	// tools/list pass through but the dispatcher must not log them.
	handler := func(_ context.Context, method string, _ json.RawMessage) (any, *rpcError) {
		switch method {
		case "initialize":
			return map[string]any{"ok": true}, nil
		case "tools/list":
			return map[string]any{"tools": []any{}}, nil
		case "tools/call":
			return map[string]any{"content": []any{}}, nil
		}
		return nil, methodNotFound(method)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, syncWriter, handler, WithAudit(audit)) }()

	// One initialize, one tools/list, one tools/call — only the last
	// should produce an audit record.
	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n"))
	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"))
	args := `{"session":"demo","keys":["echo hi","Enter"]}`
	frame := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send_keys","arguments":` + args + `}}` + "\n"
	_, _ = in.Write([]byte(frame))

	// Wait until the tools/call response shows up so we know all three
	// frames have round-tripped through the dispatcher and Audit.Record
	// has been called for the last one.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		body := rpcOut.String()
		outMu.Unlock()
		if strings.Count(body, "\n") >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	in.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after EOF")
	}

	lines := readAuditLines(t, auditBuf)
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 audit line, got %d: %s", len(lines), auditBuf.String())
	}
	rec := lines[0]

	if got, _ := rec["tool"].(string); got != "send_keys" {
		t.Errorf("tool = %q, want send_keys", got)
	}
	if got, _ := rec["session"].(string); got != "demo" {
		t.Errorf("session = %v, want demo", rec["session"])
	}
	if got, _ := rec["result"].(string); got != "ok" {
		t.Errorf("result = %q, want ok", got)
	}
	if _, present := rec["error_code"]; present {
		t.Errorf("error_code unexpectedly present on success: %v", rec["error_code"])
	}
	if got, _ := rec["request_id"].(string); got == "" {
		t.Errorf("request_id is empty: %v", rec)
	} else if len(got) != 8 {
		t.Errorf("request_id = %q, expected 8 hex chars", got)
	}
	// duration_ms is a JSON number → float64 after Unmarshal. The handler
	// is essentially instant, so we just check the field is present and
	// non-negative.
	if got, ok := rec["duration_ms"].(float64); !ok || got < 0 {
		t.Errorf("duration_ms missing or negative: %v", rec["duration_ms"])
	}
	if got, ok := rec["args_size_bytes"].(float64); !ok || int(got) != len(args) {
		t.Errorf("args_size_bytes = %v, want %d", rec["args_size_bytes"], len(args))
	}
	// ts must parse as RFC3339Nano so downstream tooling can sort
	// records lexicographically.
	tsStr, _ := rec["ts"].(string)
	if _, err := time.Parse(time.RFC3339Nano, tsStr); err != nil {
		t.Errorf("ts %q is not RFC3339Nano: %v", tsStr, err)
	}
	// CRITICAL: args content must NEVER appear in the audit record.
	if strings.Contains(auditBuf.String(), "echo hi") {
		t.Fatalf("args content leaked into audit log: %s", auditBuf.String())
	}
}

// TestAudit_ErrorPathRecordsErrorCode covers the failure side of the
// schema: when the handler returns *rpcError, result must flip to
// "error" and the error_code field must carry the JSON-RPC code. This
// is the field MCP clients branch on, so locking it down with a test
// keeps an accidental refactor from silently downgrading the audit
// signal.
func TestAudit_ErrorPathRecordsErrorCode(t *testing.T) {
	t.Parallel()
	auditBuf := &bytes.Buffer{}
	audit := newBufferAudit(auditBuf)

	in := &threadSafeBuffer{}
	rpcOut := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: rpcOut, mu: outMu}

	handler := func(_ context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{Code: -32602, Message: "bad params"}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, syncWriter, handler, WithAudit(audit)) }()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_keys","arguments":{"session":"s"}}}` + "\n"))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		body := rpcOut.String()
		outMu.Unlock()
		if strings.Contains(body, "-32602") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	in.Close()
	<-done

	lines := readAuditLines(t, auditBuf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 audit line, got %d: %s", len(lines), auditBuf.String())
	}
	rec := lines[0]
	if got, _ := rec["result"].(string); got != "error" {
		t.Fatalf("result = %q, want error", got)
	}
	code, ok := rec["error_code"].(float64)
	if !ok {
		t.Fatalf("error_code missing or wrong type: %v", rec["error_code"])
	}
	if int(code) != -32602 {
		t.Fatalf("error_code = %v, want -32602", code)
	}
}

// TestAudit_ArgsSizeBytesEqualsRawLength locks in the exact byte
// accounting promised by the schema: args_size_bytes must equal the
// raw JSON byte length of the "arguments" payload — not its decoded
// size, not its space-stripped length. Different sizes here would
// fool size-based anomaly detectors.
func TestAudit_ArgsSizeBytesEqualsRawLength(t *testing.T) {
	t.Parallel()
	auditBuf := &bytes.Buffer{}
	audit := newBufferAudit(auditBuf)

	in := &threadSafeBuffer{}
	rpcOut := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: rpcOut, mu: outMu}

	handler := func(_ context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		return map[string]any{"ok": true}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, syncWriter, handler, WithAudit(audit)) }()

	// Hand-craft args with deliberate whitespace inside so the byte
	// count differs from a "minimal" encoding. The dispatcher must
	// preserve whatever bytes the client sent, length-wise.
	args := `{ "session" : "demo" , "keys" : [ "x" , "Enter" ] }`
	frame := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_keys","arguments":` + args + `}}` + "\n"
	_, _ = in.Write([]byte(frame))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		body := rpcOut.String()
		outMu.Unlock()
		if strings.Contains(body, `"ok":true`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	in.Close()
	<-done

	lines := readAuditLines(t, auditBuf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 audit line, got %d: %s", len(lines), auditBuf.String())
	}
	rec := lines[0]
	got, ok := rec["args_size_bytes"].(float64)
	if !ok {
		t.Fatalf("args_size_bytes missing: %v", rec)
	}
	if int(got) != len(args) {
		t.Fatalf("args_size_bytes = %v, want %d (raw byte length of args)", got, len(args))
	}
}

// TestOpenAudit_DisabledReturnsNil pins the contract that the empty
// path is the audit-disabled signal: OpenAudit must return (nil, nil)
// so callers don't have to special-case it.
func TestOpenAudit_DisabledReturnsNil(t *testing.T) {
	t.Parallel()
	a, err := OpenAudit("", io.Discard)
	if err != nil {
		t.Fatalf("OpenAudit(\"\"): %v", err)
	}
	if a != nil {
		t.Fatalf("expected nil *Audit for empty path, got %v", a)
	}
}

// TestOpenAudit_StderrUsesProvidedWriter verifies the "stderr" magic
// path: records reach whatever io.Writer the caller supplied as the
// stderr argument, regardless of process-level os.Stderr.
func TestOpenAudit_StderrUsesProvidedWriter(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	a, err := OpenAudit(AuditStderr, buf)
	if err != nil {
		t.Fatalf("OpenAudit(stderr): %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	a.Record("rid", "send_keys", json.RawMessage(`{"session":"x"}`), 7*time.Millisecond, nil)

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("decode: %v body=%s", err, buf.String())
	}
	if rec["tool"] != "send_keys" {
		t.Fatalf("tool = %v, want send_keys", rec["tool"])
	}
	if rec["session"] != "x" {
		t.Fatalf("session = %v, want x", rec["session"])
	}
}

// TestOpenAudit_FilePathPersists confirms the file-path mode: records
// are written to the named file (mode 0600 / append-only), Close
// flushes the buffer so reading the file after Close shows every
// record. This is the production deployment path — operators expect
// the log to actually contain what was emitted.
func TestOpenAudit_FilePathPersists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	a, err := OpenAudit(path, io.Discard)
	if err != nil {
		t.Fatalf("OpenAudit(%q): %v", path, err)
	}
	a.Record("rid-A", "send_keys", json.RawMessage(`{"session":"alpha"}`), time.Millisecond, nil)
	a.Record("rid-B", "capture", json.RawMessage(`{"session":"beta"}`), time.Millisecond,
		&rpcError{Code: -32000, Message: "session not found"})
	if cerr := a.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	body := string(raw)
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines in %q, got %d (body=%q)", path, len(lines), body)
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decode line 0: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("decode line 1: %v", err)
	}
	if first["request_id"] != "rid-A" || first["result"] != "ok" {
		t.Errorf("line 0 unexpected: %v", first)
	}
	if second["request_id"] != "rid-B" || second["result"] != "error" {
		t.Errorf("line 1 unexpected: %v", second)
	}
	if code, _ := second["error_code"].(float64); int(code) != -32000 {
		t.Errorf("error_code = %v, want -32000", second["error_code"])
	}
}

// TestOpenAudit_OpenFailureReturnsError covers the operator-visible
// failure path: when the supplied path can't be opened (parent dir
// missing), OpenAudit must return a wrapped error so main exits
// non-zero instead of silently running with audit disabled.
func TestOpenAudit_OpenFailureReturnsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Path under a nonexistent subdir → ENOENT.
	bad := filepath.Join(dir, "no-such-dir", "audit.log")
	a, err := OpenAudit(bad, io.Discard)
	if err == nil {
		_ = a.Close()
		t.Fatalf("expected error opening %q, got nil", bad)
	}
	if !strings.Contains(err.Error(), bad) {
		t.Fatalf("error %q does not include path %q", err, bad)
	}
}

package main

import (
	"fmt"
	"io"
	"os"
)

// LogOutputStderr is the magic value for -log-output that keeps the slog
// stream on the supplied stderr writer (the default behaviour). Operators
// pass it explicitly when scripting around environments that may otherwise
// override the default.
const LogOutputStderr = "stderr"

// LogOutputStdout is the magic value for -log-output that routes slog
// output to stdout. It exists only for ad-hoc debugging in combination
// with -dry-run / -version (paths that never serve JSON-RPC frames).
// Using it while the server is actually serving stdio will interleave
// log records with JSON-RPC frames and corrupt the protocol.
const LogOutputStdout = "stdout"

// noopCloser is the closer returned for writer targets we do not own:
// stderr, stdout, and the disabled case. It mirrors the audit sink's
// "Close on a nil/borrowed sink is a no-op" contract so callers can
// always defer closer() unconditionally.
func noopCloser() error { return nil }

// openLogOutput resolves -log-output and returns the writer slog should
// install plus a closer the caller must invoke during shutdown.
//
// The two magic values:
//
//   - "" or "stderr"  → returns (stderr, noopCloser, nil). The default
//     behaviour: structured logs share the supplied stderr stream and
//     the caller does not own the fd's lifetime.
//   - "stdout"        → returns (stdout, noopCloser, nil). DANGER: this
//     interleaves log records with the JSON-RPC frames the server
//     writes when actually serving stdio. Only useful with -dry-run /
//     -version where stdout carries one well-known line.
//
// Any other value is treated as a filesystem path opened with
// O_APPEND|O_CREATE|O_WRONLY at mode 0600 — same shape as the
// audit-log sink so operator expectations transfer cleanly. The
// returned closer flushes via os.File.Sync (best-effort) and then
// closes the fd; on success the file outlives the call until the
// caller invokes the closer during shutdown.
//
// Errors carry the path so operators see exactly what was wrong (e.g.
// "open log output \"/no/such/dir/agent.log\": open …: no such file or
// directory") instead of a bare permission/missing-parent diagnostic.
func openLogOutput(target string, stderr, stdout io.Writer) (io.Writer, func() error, error) {
	switch target {
	case "", LogOutputStderr:
		return stderr, noopCloser, nil
	case LogOutputStdout:
		return stdout, noopCloser, nil
	}
	// O_APPEND keeps concurrent processes from clobbering each other's
	// records (e.g. an operator running two binaries against the same
	// log file during a rolling restart); mode 0600 keeps the log
	// private because slog records can carry hostnames, session
	// names, and timing data the operator would rather not leak via
	// a group-readable file.
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log output %q: %w", target, err)
	}
	closer := func() error {
		// Sync first so any buffered data hits disk even if the
		// process is about to exit; ignore the sync error because
		// not every backing fs supports it (e.g. /dev/null on some
		// kernels) and the subsequent Close error is what we
		// surface either way.
		_ = f.Sync()
		return f.Close()
	}
	return f, closer, nil
}

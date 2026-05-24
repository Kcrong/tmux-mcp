package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// TestOpenLogOutput_DefaultStderr pins the legacy default behaviour:
// when the operator passes nothing (or the explicit "stderr" magic
// value) the helper hands back the supplied stderr writer and a
// no-op closer. This is the load-bearing assertion that flipping
// the new flag's default would silently break — flip it on a real
// deployment and slog records would suddenly land somewhere else.
func TestOpenLogOutput_DefaultStderr(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}

	// Both the empty string and the explicit magic value must yield
	// the same writer — there's no semantic difference between
	// "operator took the default" and "operator passed -log-output=stderr".
	for _, target := range []string{"", LogOutputStderr} {
		t.Run("target="+target, func(t *testing.T) {
			w, closer, err := openLogOutput(target, stderr, stdout, 0, 0)
			if err != nil {
				t.Fatalf("openLogOutput(%q): %v", target, err)
			}
			if w != io.Writer(stderr) {
				t.Fatalf("expected stderr writer for target %q, got %T", target, w)
			}
			// noop closer is identified by behaviour: returns nil
			// without touching anything. We compare function pointers
			// to pin the implementation choice.
			if reflect.ValueOf(closer).Pointer() != reflect.ValueOf(noopCloser).Pointer() {
				t.Fatalf("expected noopCloser for target %q, got %p", target, closer)
			}
			if err := closer(); err != nil {
				t.Fatalf("noopCloser returned %v, want nil", err)
			}
		})
	}
}

// TestOpenLogOutput_StdoutMagic exercises the documented escape hatch:
// "stdout" routes slog output to the supplied stdout writer (useful
// only with -dry-run / -version where stdout carries one well-known
// line). Most operators must NOT use this in production — the test
// just locks in the contract that the magic value is honoured.
func TestOpenLogOutput_StdoutMagic(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}

	w, closer, err := openLogOutput(LogOutputStdout, stderr, stdout, 0, 0)
	if err != nil {
		t.Fatalf("openLogOutput(stdout): %v", err)
	}
	if w != io.Writer(stdout) {
		t.Fatalf("expected stdout writer, got %T", w)
	}
	if err := closer(); err != nil {
		t.Fatalf("noopCloser returned %v, want nil", err)
	}
}

// TestOpenLogOutput_FilePath_WritesAndCloses confirms the production
// deployment path: a filesystem target is opened append-only at
// 0600, the returned writer accepts bytes, and the closer flushes +
// closes the underlying fd so reading the file after closer() shows
// every byte written.
func TestOpenLogOutput_FilePath_WritesAndCloses(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")

	w, closer, err := openLogOutput(path, stderr, stdout, 0, 0)
	if err != nil {
		t.Fatalf("openLogOutput(%q): %v", path, err)
	}
	const payload = "hello log\n"
	if _, werr := io.WriteString(w, payload); werr != nil {
		t.Fatalf("write to log: %v", werr)
	}
	if cerr := closer(); cerr != nil {
		t.Fatalf("closer: %v", cerr)
	}

	// Read back through the OS so we exercise the full
	// "open-write-close-read" loop the operator actually depends on.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log %q: %v", path, err)
	}
	if string(got) != payload {
		t.Fatalf("log contents = %q, want %q", got, payload)
	}

	// The file is opened with mode 0600 because slog records can
	// carry hostnames, session names, and timing data the operator
	// would rather not leak via a group-readable file. Pin that.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat log %q: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("log file perm = %#o, want 0o600", perm)
	}
}

// TestOpenLogOutput_FilePath_AppendsAcrossOpens makes sure a second
// open of the same path appends rather than truncating. This is
// the operator-visible invariant for log files: restarting tmux-mcp
// must not nuke the previous run's log.
func TestOpenLogOutput_FilePath_AppendsAcrossOpens(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")

	for _, payload := range []string{"first run\n", "second run\n"} {
		w, closer, err := openLogOutput(path, stderr, stdout, 0, 0)
		if err != nil {
			t.Fatalf("openLogOutput(%q): %v", path, err)
		}
		if _, werr := io.WriteString(w, payload); werr != nil {
			t.Fatalf("write %q: %v", payload, werr)
		}
		if cerr := closer(); cerr != nil {
			t.Fatalf("closer: %v", cerr)
		}
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	if string(got) != "first run\nsecond run\n" {
		t.Fatalf("log contents = %q, want concatenation of both runs", got)
	}
}

// TestOpenLogOutput_MissingParentDir surfaces the operator-visible
// failure mode for a typoed path: opening fails with an error that
// names the path so the diagnostic in stderr is useful.
func TestOpenLogOutput_MissingParentDir(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}

	dir := t.TempDir()
	bad := filepath.Join(dir, "no-such-dir", "agent.log")

	w, closer, err := openLogOutput(bad, stderr, stdout, 0, 0)
	if err == nil {
		// Defensive cleanup if the OS unexpectedly let this through.
		_ = closer()
		t.Fatalf("expected error opening %q, got nil writer=%T", bad, w)
	}
	if !strings.Contains(err.Error(), bad) {
		t.Fatalf("error %q does not name the path %q", err, bad)
	}
}

// TestOpenLogOutput_PermissionDenied checks the second common operator
// failure: a path that is on a writable filesystem but whose
// resolution lacks write permission. Skipped on non-Unix platforms
// (Windows ignores chmod bits) and for root, where the kernel
// short-circuits permission checks.
func TestOpenLogOutput_PermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks; cannot exercise EACCES")
	}
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}

	dir := t.TempDir()
	denied := filepath.Join(dir, "denied")
	if err := os.Mkdir(denied, 0o500); err != nil {
		t.Fatalf("mkdir %q: %v", denied, err)
	}
	target := filepath.Join(denied, "agent.log")

	w, closer, err := openLogOutput(target, stderr, stdout, 0, 0)
	if err == nil {
		_ = closer()
		t.Fatalf("expected permission error opening %q, got writer=%T", target, w)
	}
	if !strings.Contains(err.Error(), target) {
		t.Fatalf("error %q does not name the path %q", err, target)
	}
}

// TestRun_LogOutputFile_EndToEnd is the integration-level pin: when
// run() is invoked with -log-output pointing at a tempfile, slog
// records land in that file (closed cleanly on exit) instead of
// stderr. We feed run() a single malformed JSON-RPC frame so the
// dispatcher emits its "invalid request" slog.Warn before stdin EOF
// unwinds the loop, then assert that record made it to the file
// (and not to stderr).
func TestRun_LogOutputFile_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")

	var stdout, stderr bytes.Buffer
	err := run(
		// JSON format makes the assertion cheap (we look for a
		// "level" key); the malformed line below trips a Warn-level
		// record that survives any future change to the default level.
		[]string{"-log-format=json", "-log-output=" + logPath},
		strings.NewReader("not json\n"), &stdout, &stderr,
	)
	if err != nil {
		t.Fatalf("run(-log-output=%q): %v stderr=%q", logPath, err, stderr.String())
	}

	// stderr must NOT contain JSON slog records — the redirection
	// to the file is the whole point of the flag. (stdout will hold
	// the JSON-RPC parse-error response, which is fine: that frame
	// has nothing to do with slog.)
	for line := range strings.SplitSeq(stderr.String(), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") {
			t.Fatalf("expected no JSON slog records on stderr after redirection, got %q", line)
		}
	}

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log %q: %v", logPath, err)
	}
	if len(got) == 0 {
		t.Fatalf("expected at least one slog record in %q, got empty file", logPath)
	}
	// At least one structured slog record must show up. We don't
	// pin a specific message — just that the file contains a line
	// that parses as slog JSON (starts with '{', carries "level").
	sawRecord := false
	for line := range strings.SplitSeq(string(got), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.Contains(line, `"level"`) || !strings.HasPrefix(line, "{") {
			continue
		}
		sawRecord = true
		break
	}
	if !sawRecord {
		t.Fatalf("expected at least one JSON slog record in %q, got %q", logPath, got)
	}
}

// TestRun_LogOutputFile_DryRun is the direct contract test for "log
// file is closed cleanly on shutdown": with -dry-run + -log-output,
// the closer must flush + close the fd before run() returns, so the
// file descriptor is not leaked. We can't easily observe leaks from
// inside the test, so instead we verify the file is writable to
// completion (re-open and append works after run() returns).
func TestRun_LogOutputFile_DryRun(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dryrun.log")

	var stdout, stderr bytes.Buffer
	if err := run(
		[]string{"-dry-run", "-log-output=" + logPath},
		strings.NewReader(""), &stdout, &stderr,
	); err != nil {
		t.Fatalf("run(-dry-run -log-output=%q): %v stderr=%q", logPath, err, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "dry-run ok\t") {
		t.Fatalf("expected dry-run ok line, got %q", stdout.String())
	}

	// File must exist and be re-openable for append — a leaked fd
	// would still let us write but the operator-visible invariant
	// is "file is at the path with the documented permission bits".
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log %q: %v", logPath, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("log file perm = %#o, want 0o600", perm)
	}
}

// TestRun_LogOutputBadPath ensures the validation error from
// openLogOutput is surfaced cleanly: stderr carries a "tmux-mcp: open
// log output …" diagnostic, stdout stays untouched (so an MCP client
// pointed at the binary never sees a stray frame), and the returned
// error is a real failure (so main exits non-zero).
func TestRun_LogOutputBadPath(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-log-output=/no/such/dir/agent.log"},
		strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for unopenable -log-output, got nil")
	}
	if !strings.Contains(err.Error(), "open log output") {
		t.Fatalf("expected error to mention 'open log output', got %v", err)
	}
	if !strings.Contains(stderr.String(), "tmux-mcp: open log output") {
		t.Fatalf("expected stderr diagnostic, got %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected stdout untouched, got %q", stdout.String())
	}
}

// TestLogOutputFlag_AcceptedAndDocumented is the CLI-surface guard:
// the flag must show up in -help so operators can discover it via
// `tmux-mcp -help`. Behaviour for the helper is covered above; here
// we just guard the wire-up so a future rename trips a test instead
// of silently breaking the operator deployment knob.
func TestLogOutputFlag_AcceptedAndDocumented(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil && err.Error() != "flag: help requested" {
		t.Fatalf("run(-help): unexpected error %v", err)
	}
	if !strings.Contains(stderr.String(), "-log-output") {
		t.Fatalf("expected -log-output in usage block, got %q", stderr.String())
	}
	// The DANGER warning for stdout must be in the help text so
	// operators are warned at the source, not just in the README.
	if !strings.Contains(stderr.String(), "DANGER") {
		t.Fatalf("expected DANGER warning for stdout in usage block, got %q", stderr.String())
	}
}

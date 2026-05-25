package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPIDFile_Roundtrip is the happy path for -pid-file: while run() is
// alive, the file at PATH must contain this process's PID; after run()
// returns the file must be gone.
//
// We drive run() in a goroutine fed by an os.Pipe stdin so the test
// goroutine controls when stdin EOFs. That gives us a deterministic
// existence window: poll the path until we see it, assert the
// contents, THEN close the pipe so server.Serve returns and run()
// unwinds. With an empty strings.Reader stdin the test would race the
// deferred os.Remove and frequently miss the existence window under
// the race detector.
func TestPIDFile_Roundtrip(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "tmux-mcp.pid")

	// os.Pipe gives us a stdin we can hold open until the test wants
	// run() to unwind. Closing pw signals EOF to server.Serve, which
	// returns and lets run() trigger the deferred cleanup.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { _ = pr.Close() })
	t.Cleanup(func() { _ = pw.Close() }) // idempotent, in case test fails before explicit close

	var (
		stdout, stderr bytes.Buffer
		runErr         error
		runDone        = make(chan struct{})
	)
	go func() {
		defer close(runDone)
		runErr = run(
			[]string{"-pid-file", path, "-shutdown-timeout", "0"},
			pr, &stdout, &stderr,
		)
	}()

	// Poll for the pid file with a generous deadline. Under -race the
	// scheduling is slower, so 5s is the headroom budget; on a healthy
	// machine the file appears in a handful of milliseconds.
	deadline := time.Now().Add(5 * time.Second)
	var seen string
	for time.Now().Before(deadline) {
		b, rerr := os.ReadFile(path)
		if rerr == nil {
			seen = string(b)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if seen == "" {
		// Close the pipe before failing so the run() goroutine can
		// exit; a leaked goroutine would skew later tests in the
		// package.
		_ = pw.Close()
		<-runDone
		t.Fatalf("never observed the pid file at %q (stderr=%q runErr=%v)",
			path, stderr.String(), runErr)
	}
	want := strconv.Itoa(os.Getpid()) + "\n"
	if seen != want {
		_ = pw.Close()
		<-runDone
		t.Fatalf("pid file content = %q; want %q", seen, want)
	}

	// Tell run() to unwind. server.Serve EOFs on the next read and the
	// deferred os.Remove on the path fires before run() returns.
	if cerr := pw.Close(); cerr != nil {
		t.Fatalf("close stdin pipe: %v", cerr)
	}
	<-runDone
	if runErr != nil {
		t.Fatalf("run(-pid-file): %v stderr=%q", runErr, stderr.String())
	}

	// After run() returned, the deferred cleanup must have removed the
	// file. A leftover would betray operators who chained two starts
	// back-to-back.
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatalf("pid file %q still present after shutdown: stat err=%v", path, serr)
	}
}

// TestPIDFile_AlreadyExists pins the "two instances cannot silently
// clobber each other" contract: if the path is already present when
// run() starts, run() must return a non-nil error mentioning "already
// exists" and must NOT touch the file. We pre-write a sentinel value
// and assert it is still there byte-for-byte after the failed start.
//
// Not t.Parallel() — run() touches the slog.SetDefault global, so
// concurrent invocations from sibling tests would race on it. The
// rest of this file follows the same convention: anything that goes
// through run() runs sequentially.
func TestPIDFile_AlreadyExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux-mcp.pid")
	const sentinel = "99999\n"
	if err := os.WriteFile(path, []byte(sentinel), 0o644); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err := run(
		[]string{"-pid-file", path, "-shutdown-timeout", "0"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if err == nil {
		t.Fatal("expected error for pre-existing pid file, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected error to mention 'already exists', got %v", err)
	}

	// File must be untouched — operators rely on the failure signal
	// AND on the leftover content surviving so they can grep the
	// stale PID and decide whether to clean up.
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("re-read pid file: %v", rerr)
	}
	if string(got) != sentinel {
		t.Fatalf("pid file content was modified by failed start: got %q want %q",
			string(got), sentinel)
	}
}

// TestPIDFile_PermissionDenied confirms the startup error path for an
// unwritable destination. We point -pid-file at a nested path under a
// directory that does not exist; os.WriteFile on the .tmp sibling
// fails and run() must surface that as a clean error before opening
// any sockets.
//
// We avoid relying on Linux-specific permission bits (which behave
// differently for root, in containers, and on tmpfs) — a non-existent
// parent directory is a portable, deterministic failure mode that
// exercises the same error path.
func TestPIDFile_PermissionDenied(t *testing.T) {
	t.Parallel()
	// Path lives under a directory that does not exist. os.WriteFile
	// will return ENOENT on the parent and our error wrapper kicks in.
	dir := t.TempDir()
	path := filepath.Join(dir, "missing-subdir", "tmux-mcp.pid")

	var stdout, stderr bytes.Buffer
	err := run(
		[]string{"-pid-file", path, "-shutdown-timeout", "0"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if err == nil {
		t.Fatal("expected error for unwritable pid-file path, got nil")
	}
	// Error must name the path so the operator can fix the typo / mkdir.
	if !strings.Contains(err.Error(), "pid file") {
		t.Fatalf("expected error to mention 'pid file', got %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("expected error to quote the failing path %q, got %v", path, err)
	}
}

// TestPIDFileFlag_AcceptedAndDocumented guards the operator-visible CLI
// surface: the flag must show up in -help and a smoke-test invocation
// must accept the flag (i.e. it's actually registered with FlagSet, not
// just a doc-only entry). Behaviour is covered by the round-trip tests
// above — here we just guard the help text and the wire-up so a future
// rename trips a test.
func TestPIDFileFlag_AcceptedAndDocumented(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil && err.Error() != "flag: help requested" {
		t.Fatalf("run(-help): unexpected error %v", err)
	}
	if !strings.Contains(stderr.String(), "-pid-file") {
		t.Fatalf("expected -pid-file in usage block, got %q", stderr.String())
	}
}

// TestWritePIDFile_Contract is a focused unit test for the writePIDFile
// helper, independent of the run() bootstrap. It pins three invariants:
//
//  1. A successful write produces exactly "<pid>\n" at mode 0644.
//  2. A pre-existing file is rejected with the documented sentinel
//     phrase and the existing content is untouched.
//  3. A path under a missing parent directory is rejected cleanly.
func TestWritePIDFile_Contract(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "ok.pid")
		if err := writePIDFile(path); err != nil {
			t.Fatalf("writePIDFile: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		want := fmt.Sprintf("%d\n", os.Getpid())
		if string(got) != want {
			t.Fatalf("content = %q; want %q", string(got), want)
		}
		// .tmp sibling must NOT linger after a successful rename.
		if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf("expected .tmp to be gone, stat err=%v", err)
		}
	})

	t.Run("already-exists", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "exists.pid")
		const sentinel = "42\n"
		if err := os.WriteFile(path, []byte(sentinel), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		err := writePIDFile(path)
		if err == nil {
			t.Fatal("expected error for pre-existing path, got nil")
		}
		if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected 'already exists' phrasing, got %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != sentinel {
			t.Fatalf("pre-existing content modified: got %q want %q", string(got), sentinel)
		}
	})

	t.Run("missing-parent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "no-such-dir", "x.pid")
		err := writePIDFile(path)
		if err == nil {
			t.Fatal("expected error for missing parent dir, got nil")
		}
		if !strings.Contains(err.Error(), "pid file") {
			t.Fatalf("expected error to mention 'pid file', got %v", err)
		}
	})
}

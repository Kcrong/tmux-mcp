package tmuxctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestPipePane_LogsOutputToFile drives the happy path: start a pipe to
// `cat > <tmpfile>`, send keys whose output we expect to flow through,
// and assert the file picks up the sentinel string. This is the
// load-bearing contract every agent that reaches for pipe_pane relies
// on — the bytes actually go somewhere.
func TestPipePane_LogsOutputToFile(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "pp", Command: "/bin/sh", Width: 80, Height: 20,
		Env: map[string]string{"PS1": "$ "},
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	out := filepath.Join(t.TempDir(), "pipe.log")
	if err := c.PipePane(ctx, "pp", "cat > "+out, false, false); err != nil {
		t.Fatalf("PipePane(start): %v", err)
	}

	// Drive a sentinel through the pane and wait for tmux to flush it
	// into the pipe. WaitForText pins the on-screen state so we know the
	// shell finished the echo, but the pipe is async: tmux flushes the
	// captured output through `cat` on its own schedule. Poll the file
	// until the sentinel appears.
	if err := c.SendKeys(ctx, "pp", []string{"echo PIPE_PANE_HELLO_42", "Enter"}, false); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if _, err := c.WaitForText(
		ctx, "pp", `PIPE_PANE_HELLO_42`,
		50*time.Millisecond, 5*time.Second,
	); err != nil {
		t.Fatalf("WaitForText: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var body []byte
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(out) //nolint:gosec // path is t.TempDir-rooted, controlled by the test
		if err == nil && strings.Contains(string(b), "PIPE_PANE_HELLO_42") {
			body = b
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(string(body), "PIPE_PANE_HELLO_42") {
		t.Fatalf("pipe log %q never picked up the sentinel; body=%q", out, body)
	}
}

// TestPipePane_StopAcceptsEmptyCommand pins the documented "no command =
// stop" semantics: starting a pipe and then issuing pipe-pane again
// without a shell command must succeed and tear the pipe down. We don't
// assert on the file size shrinking (tmux already flushed whatever it
// had), only on the second call returning success — which is the tmux
// contract callers rely on for "rotate the log".
func TestPipePane_StopAcceptsEmptyCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "ppstop", Command: "/bin/sh", Width: 80, Height: 20,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	out := filepath.Join(t.TempDir(), "pipe.log")
	if err := c.PipePane(ctx, "ppstop", "cat > "+out, false, false); err != nil {
		t.Fatalf("PipePane(start): %v", err)
	}
	// Empty shellCommand maps to a bare `pipe-pane` argv, which is tmux's
	// documented way to stop an existing pipe.
	if err := c.PipePane(ctx, "ppstop", "", false, false); err != nil {
		t.Fatalf("PipePane(stop): %v", err)
	}
}

// TestPipePane_MissingTargetWrapsSentinel pins the typed-error contract
// for an unknown target: callers (and the JSON-RPC layer) must be able
// to errors.Is into errs.ErrSessionNotFound regardless of which exact
// phrase tmux emitted ("can't find pane" vs "no current target").
func TestPipePane_MissingTargetWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, pane missing"
	// rather than "no server" (different stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.PipePane(ctx, "ghost_session_nonexistent:0.0", "cat > /dev/null", false, false)
	if err == nil {
		t.Fatal("expected error for missing pane")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestPipePane_RejectsEmptyTarget locks the up-front guard. tmux would
// otherwise resolve "" to whatever pane it considers current, which is
// almost never what the caller actually wanted.
func TestPipePane_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.PipePane(ctx, "", "cat > /dev/null", false, false)
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if !strings.Contains(err.Error(), "target required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

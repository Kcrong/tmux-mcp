package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestClearHistory_DropsScrollback runs the happy path: create a small
// session, fill its scrollback with hundreds of lines, capture the
// scrollback to confirm the buffer is non-trivial, call ClearHistory,
// and assert the post-call scrollback is back to (near-)empty. This is
// the load-bearing contract every agent that reaches for clear_history
// relies on — the buffer actually goes away.
func TestClearHistory_DropsScrollback(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "ch", Command: "/bin/sh",
		// Small terminal so a few hundred echoes overflow the visible
		// region into scrollback quickly.
		Width: 80, Height: 10,
		Env: map[string]string{"PS1": "$ "},
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Drive a tight loop that prints far more lines than the height so
	// the visible region scrolls off into the scrollback buffer.
	if err := c.SendKeys(ctx, "ch",
		[]string{"for i in $(seq 1 500); do echo line-$i; done", "Enter"}, false,
	); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	// Give the shell a moment to actually run the loop and have its
	// stdout absorbed into tmux's pty before the first capture. Without
	// this, the scrollback may still be small when we sample it.
	if _, err := c.WaitForText(ctx, "ch", `line-500`,
		50*time.Millisecond, 5*time.Second,
	); err != nil {
		t.Fatalf("WaitForText line-500: %v", err)
	}

	pre, err := c.Capture(ctx, "ch", CaptureScrollback, false)
	if err != nil {
		t.Fatalf("Capture pre-clear: %v", err)
	}
	preLines := strings.Count(pre, "\n")
	// We dumped 500 lines into a 10-row pane. Anything substantially
	// larger than the visible region proves scrollback is populated.
	if preLines < 100 {
		t.Fatalf("scrollback pre-clear has only %d lines (body=%q); expected >=100", preLines, pre)
	}

	if clearErr := c.ClearHistory(ctx, "ch"); clearErr != nil {
		t.Fatalf("ClearHistory: %v", clearErr)
	}

	post, err := c.Capture(ctx, "ch", CaptureScrollback, false)
	if err != nil {
		t.Fatalf("Capture post-clear: %v", err)
	}
	// After clear-history the scrollback should be gone — only the
	// visible region (bounded by Height=10) remains. Allow a small
	// margin for tmux's own bookkeeping rows.
	postLines := strings.Count(post, "\n")
	if postLines >= preLines {
		t.Fatalf("scrollback post-clear (%d lines) did not shrink from pre (%d lines)", postLines, preLines)
	}
	if postLines > 30 {
		t.Fatalf("scrollback post-clear has %d lines, expected <=30 (pane height ~10)", postLines)
	}
}

// TestClearHistory_MissingTargetWrapsSentinel pins the typed-error
// contract for an unknown target: callers (and the JSON-RPC layer) must
// be able to errors.Is into errs.ErrSessionNotFound regardless of which
// exact phrase tmux emitted ("can't find pane" vs "no current target").
func TestClearHistory_MissingTargetWrapsSentinel(t *testing.T) {
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

	err := c.ClearHistory(ctx, "ghost_session_nonexistent:0.0")
	if err == nil {
		t.Fatal("expected error for missing pane")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestClearHistory_RejectsEmptyTarget locks the up-front guard. tmux
// would otherwise resolve "" to whatever pane it considers current,
// which is almost never what the caller actually wanted.
func TestClearHistory_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.ClearHistory(ctx, "")
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if !strings.Contains(err.Error(), "target required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

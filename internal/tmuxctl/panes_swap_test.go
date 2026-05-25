package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSwapPane_SwapsTwoPanes drives the happy path: split the session
// into two panes, write a distinguishing sentinel into each via
// send-keys, then SwapPane and assert the per-position captures swap.
// pane_id stays glued to its contents (tmux moves layout slots, not
// buffers), so the assertion is "what's at position 0 and 1 changed
// places".
func TestSwapPane_SwapsTwoPanes(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "swp", Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := c.SplitPane(ctx, SplitOptions{
		Session:   "swp",
		Direction: "vertical",
		Detach:    true,
	}); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}

	// Tag each pane with a distinct sentinel so the swap is observable
	// via capture-pane against the same position target.
	if err := c.SendKeys(ctx, "swp:0.0", []string{"echo PANE_ZERO_MARK", "Enter"}, false); err != nil {
		t.Fatalf("SendKeys pane 0: %v", err)
	}
	if err := c.SendKeys(ctx, "swp:0.1", []string{"echo PANE_ONE_MARK", "Enter"}, false); err != nil {
		t.Fatalf("SendKeys pane 1: %v", err)
	}

	// Wait for both shells to settle so the sentinels are committed to
	// the visible region before we capture.
	waitForText := func(target, want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			out, err := c.run(ctx, "capture-pane", "-p", "-t", target)
			if err != nil {
				t.Fatalf("capture-pane %s: %v", target, err)
			}
			if strings.Contains(out, want) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%q never appeared at %s", want, target)
	}
	waitForText("swp:0.0", "PANE_ZERO_MARK")
	waitForText("swp:0.1", "PANE_ONE_MARK")

	if err := c.SwapPane(ctx, "swp:0.0", "swp:0.1"); err != nil {
		t.Fatalf("SwapPane: %v", err)
	}

	// After the swap, position 0 must show what used to be at position 1
	// and vice versa.
	zeroAfter, err := c.run(ctx, "capture-pane", "-p", "-t", "swp:0.0")
	if err != nil {
		t.Fatalf("capture-pane swp:0.0 after swap: %v", err)
	}
	oneAfter, err := c.run(ctx, "capture-pane", "-p", "-t", "swp:0.1")
	if err != nil {
		t.Fatalf("capture-pane swp:0.1 after swap: %v", err)
	}
	if !strings.Contains(zeroAfter, "PANE_ONE_MARK") {
		t.Fatalf("after swap, pane 0 missing PANE_ONE_MARK:\n%s", zeroAfter)
	}
	if !strings.Contains(oneAfter, "PANE_ZERO_MARK") {
		t.Fatalf("after swap, pane 1 missing PANE_ZERO_MARK:\n%s", oneAfter)
	}
}

// TestSwapPane_MissingSessionWrapsSentinel pins the typed sentinel so the
// JSON-RPC layer can map "session/pane not found" to CodeSessionNotFound
// — the same contract every other tmuxctl pane method upholds.
func TestSwapPane_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, session
	// missing" (the stderr shape changes versus "no server").
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.SwapPane(ctx, "ghost_session_nonexistent:0.0", "anchor:0.0")
	if err == nil {
		t.Fatal("expected error for missing src session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSwapPane_RejectsEmptySrc locks the up-front guard. tmux would
// otherwise resolve "" to whatever pane it considers current.
func TestSwapPane_RejectsEmptySrc(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.SwapPane(ctx, "", "demo:0.1")
	if err == nil {
		t.Fatal("expected error for empty src")
	}
	if !strings.Contains(err.Error(), "src required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSwapPane_RejectsEmptyDst mirrors the src guard for the destination
// argument so a half-formed call cannot reach tmux.
func TestSwapPane_RejectsEmptyDst(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.SwapPane(ctx, "demo:0.0", "")
	if err == nil {
		t.Fatal("expected error for empty dst")
	}
	if !strings.Contains(err.Error(), "dst required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

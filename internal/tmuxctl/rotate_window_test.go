package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// listPaneIDs runs `tmux list-panes -F '#{pane_id}' -t <target>` and
// returns the IDs in their current slot order. RotateWindow leaves the
// pane buffers and tmux pane ids untouched and only shifts which pane
// occupies which layout slot, so capturing the ID ordering before /
// after the rotation is the most direct way to prove the rotation
// landed.
func listPaneIDs(t *testing.T, c *Controller, ctx context.Context, target string) []string {
	t.Helper()
	out, err := c.run(ctx, "list-panes", "-t", target, "-F", "#{pane_id}")
	if err != nil {
		t.Fatalf("list-panes %s: %v", target, err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// buildThreePaneWindow splits the active window of session into three
// panes running /bin/bash. Each call to SplitPane lands a new pane in a
// fresh layout slot, so the resulting window has three distinct
// pane_ids that RotateWindow can shuffle. Returns the captured slot
// ordering so the caller can assert against it after the rotation.
func buildThreePaneWindow(t *testing.T, c *Controller, ctx context.Context, session string) []string {
	t.Helper()
	if err := c.CreateSession(ctx, SessionSpec{
		Name: session, Command: "/bin/bash", Width: 100, Height: 30,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.SplitPane(ctx, SplitOptions{
		Session: session, Direction: "horizontal", Command: "/bin/bash", Detach: true,
	}); err != nil {
		t.Fatalf("SplitPane #1: %v", err)
	}
	if _, err := c.SplitPane(ctx, SplitOptions{
		Session: session, Direction: "vertical", Command: "/bin/bash", Detach: true,
	}); err != nil {
		t.Fatalf("SplitPane #2: %v", err)
	}
	ids := listPaneIDs(t, c, ctx, session)
	if len(ids) != 3 {
		t.Fatalf("baseline pane count = %d, want 3 (ids=%v)", len(ids), ids)
	}
	return ids
}

// TestRotateWindow_UpwardShiftsByOne is the load-bearing happy path.
// With three panes A B C in slot order, the tmux default `-U` rotation
// shifts every pane "up" one slot so position 0 hosts the pane that
// used to be at position 1, and so on; the pane that used to be at the
// last slot wraps back to slot 0. We pin that exact rotation against
// the captured pane_id sequence — anything else (no movement, two
// slots, reversed direction) trips the assertion.
func TestRotateWindow_UpwardShiftsByOne(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	pre := buildThreePaneWindow(t, c, ctx, "rotu")

	if err := c.RotateWindow(ctx, "rotu", false); err != nil {
		t.Fatalf("RotateWindow upward: %v", err)
	}

	post := listPaneIDs(t, c, ctx, "rotu")
	if len(post) != len(pre) {
		t.Fatalf("post pane count = %d, want %d (post=%v pre=%v)", len(post), len(pre), post, pre)
	}
	// `-U` shifts panes "up" through the slots: the pane that was at
	// position i+1 lands at position i, and the last pane wraps back to
	// position 0. Equivalently, post[i] == pre[(i+1) % n].
	for i := range pre {
		want := pre[(i+1)%len(pre)]
		if post[i] != want {
			t.Fatalf("upward rotate: post[%d]=%q, want %q (pre=%v post=%v)",
				i, post[i], want, pre, post)
		}
	}
}

// TestRotateWindow_DownwardShiftsByOneOtherWay pins the `-D` flag
// plumbing. Without this test, a refactor that always emitted `-U`
// would still pass the upward case (because tmux's default *is* `-U`)
// — the only way to prove the boolean reaches tmux is to assert the
// *opposite* shift here.
func TestRotateWindow_DownwardShiftsByOneOtherWay(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	pre := buildThreePaneWindow(t, c, ctx, "rotd")

	if err := c.RotateWindow(ctx, "rotd", true); err != nil {
		t.Fatalf("RotateWindow downward: %v", err)
	}

	post := listPaneIDs(t, c, ctx, "rotd")
	if len(post) != len(pre) {
		t.Fatalf("post pane count = %d, want %d (post=%v pre=%v)", len(post), len(pre), post, pre)
	}
	// `-D` is the inverse: post[i] == pre[(i-1+n) % n] — every pane
	// shifts "down" one slot, with the pane at slot 0 wrapping to the
	// last slot. We compute the index with +n before % to keep the
	// arithmetic positive on Go's truncated-modulo semantics.
	for i := range pre {
		want := pre[(i-1+len(pre))%len(pre)]
		if post[i] != want {
			t.Fatalf("downward rotate: post[%d]=%q, want %q (pre=%v post=%v)",
				i, post[i], want, pre, post)
		}
	}
}

// TestRotateWindow_MissingSessionWrapsSentinel pins the typed-error
// flow: RotateWindow against an unknown session must surface
// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
// CodeSessionNotFound — same contract as SwapWindow / SelectWindow.
func TestRotateWindow_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	// Anchor a real session so we exercise the "server up, session
	// missing" path. Without it, tmux emits "no server running" instead
	// of "can't find window", which would land on a different code path.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.RotateWindow(ctx, "ghost_session_nonexistent", false)
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestRotateWindow_RejectsEmptyTarget locks the up-front empty-string
// guard: a `tmux rotate-window` is never issued with no -t so tmux can
// not silently fall back to "the current window of the current client".
func TestRotateWindow_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.RotateWindow(ctx, "", false)
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if got := err.Error(); got != "target required" {
		t.Fatalf("error = %q, want %q", got, "target required")
	}
}

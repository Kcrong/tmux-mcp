package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestJoinPane_MovesPaneIntoDestinationWindow drives the happy path:
// start a session with two windows (each holding a single pane), then
// JoinPane the only pane out of window 1 and into window 0. After the
// join, window 0 must hold two panes and window 1 must be gone — tmux
// reaps a window once its last pane has been pulled out, which is the
// observable contract callers depend on.
func TestJoinPane_MovesPaneIntoDestinationWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "jp", Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// new-window gives us a second window (jp:1) holding a single
	// pane (jp:1.0) — the donor we'll move back into window 0. We don't
	// go through CreateWindow's higher-level wrapper here because the
	// controller is the unit under test — the raw run() keeps the
	// helper surface to just JoinPane / ListPanes / ListWindows.
	if _, err := c.run(ctx, "new-window", "-d", "-t", "jp:"); err != nil {
		t.Fatalf("new-window: %v", err)
	}

	// Sanity: before the join, the session has two windows.
	winsBefore, err := c.ListWindows(ctx, "jp")
	if err != nil {
		t.Fatalf("ListWindows before: %v", err)
	}
	if len(winsBefore) != 2 {
		t.Fatalf("ListWindows before join returned %d windows, want 2", len(winsBefore))
	}

	// Move the donor pane (jp:1.0) into window 0. tmux's default
	// (no -h) is the top/bottom split — pass horizontal=false to
	// assert the default flag selection.
	if err = c.JoinPane(ctx, "jp:1.0", "jp:0", false); err != nil {
		t.Fatalf("JoinPane: %v", err)
	}

	// Window 0 must now have 2 panes; window 1 must be gone (its only
	// pane was moved away, and tmux reaps the empty window).
	panesZero, err := c.ListPanes(ctx, "jp:0")
	if err != nil {
		t.Fatalf("ListPanes jp:0: %v", err)
	}
	if len(panesZero) != 2 {
		t.Fatalf("ListPanes jp:0 returned %d panes, want 2", len(panesZero))
	}
	winsAfter, err := c.ListWindows(ctx, "jp")
	if err != nil {
		t.Fatalf("ListWindows after: %v", err)
	}
	if len(winsAfter) != 1 {
		t.Fatalf("ListWindows after join returned %d windows, want 1", len(winsAfter))
	}
	if winsAfter[0].Index != 0 {
		t.Fatalf("ListWindows after join: surviving window is index %d, want 0", winsAfter[0].Index)
	}
}

// TestJoinPane_HorizontalUsesHFlag asserts that horizontal=true reaches
// tmux as `-h`, producing a side-by-side split rather than the default
// top/bottom one. We observe this through the resulting panes' widths:
// after a horizontal join the two panes share the row, so each is
// strictly narrower than the full-width window, while a vertical join
// leaves both at full width.
func TestJoinPane_HorizontalUsesHFlag(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	const cols = 80
	if err := c.CreateSession(ctx, SessionSpec{
		Name: "jph", Command: "/bin/sh", Width: cols, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.run(ctx, "new-window", "-d", "-t", "jph:"); err != nil {
		t.Fatalf("new-window: %v", err)
	}

	if err := c.JoinPane(ctx, "jph:1.0", "jph:0", true); err != nil {
		t.Fatalf("JoinPane horizontal: %v", err)
	}

	panes, err := c.ListPanes(ctx, "jph:0")
	if err != nil {
		t.Fatalf("ListPanes after horizontal join: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("ListPanes after horizontal join returned %d panes, want 2", len(panes))
	}
	// A horizontal split shares the row between two panes, so each
	// must be strictly narrower than the full-width window. Use a loose
	// bound (< cols) rather than exactly cols/2 because tmux reserves
	// one column for the divider.
	for _, p := range panes {
		if p.Width >= cols {
			t.Fatalf("pane %s width=%d, want <%d after horizontal join (panes should share the row)",
				p.ID, p.Width, cols)
		}
	}
}

// TestJoinPane_MissingSessionWrapsSentinel pins the typed sentinel so
// the JSON-RPC layer can map "session/pane not found" to
// CodeSessionNotFound — the same contract every other tmuxctl pane
// method upholds.
func TestJoinPane_MissingSessionWrapsSentinel(t *testing.T) {
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

	err := c.JoinPane(ctx, "ghost_session_nonexistent:0.0", "anchor:0", false)
	if err == nil {
		t.Fatal("expected error for missing src session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestJoinPane_RejectsEmptySrc locks the up-front guard. tmux would
// otherwise resolve "" to whatever pane it considers current.
func TestJoinPane_RejectsEmptySrc(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.JoinPane(ctx, "", "demo:0", false)
	if err == nil {
		t.Fatal("expected error for empty src")
	}
	if !strings.Contains(err.Error(), "src required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestJoinPane_RejectsEmptyDst mirrors the src guard for the destination
// argument so a half-formed call cannot reach tmux.
func TestJoinPane_RejectsEmptyDst(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.JoinPane(ctx, "demo:0.0", "", false)
	if err == nil {
		t.Fatal("expected error for empty dst")
	}
	if !strings.Contains(err.Error(), "dst required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

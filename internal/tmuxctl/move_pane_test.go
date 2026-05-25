package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestMovePane_RelocatesAcrossWindows drives the happy path: a session
// with two windows (each with a single pane) gets the lone pane of the
// second window moved into the first window via MovePane. After the
// move the donor window must be reaped (tmux drops empty windows) and
// the destination window must report two panes.
func TestMovePane_RelocatesAcrossWindows(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "mpacross", Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "mpacross", Name: "donor", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	// Sanity: two windows, each with a single pane.
	winsBefore, err := c.ListWindows(ctx, "mpacross")
	if err != nil {
		t.Fatalf("ListWindows pre-move: %v", err)
	}
	if len(winsBefore) != 2 {
		t.Fatalf("ListWindows pre-move = %d, want 2", len(winsBefore))
	}

	// Move the only pane of window 1 into window 0. tmux reaps window 1
	// once it has no panes left.
	if mvErr := c.MovePane(ctx, "mpacross:1.0", "mpacross:0.0", false, false, true); mvErr != nil {
		t.Fatalf("MovePane: %v", mvErr)
	}

	winsAfter, err := c.ListWindows(ctx, "mpacross")
	if err != nil {
		t.Fatalf("ListWindows post-move: %v", err)
	}
	if len(winsAfter) != 1 {
		t.Fatalf("ListWindows post-move = %d, want 1 (donor should be reaped)", len(winsAfter))
	}
	dstPanes, err := c.ListPanes(ctx, "mpacross:0")
	if err != nil {
		t.Fatalf("ListPanes destination: %v", err)
	}
	if len(dstPanes) != 2 {
		t.Fatalf("destination pane count = %d, want 2", len(dstPanes))
	}
}

// TestMovePane_HorizontalSplitsLeftRight pins the `-h` flag end-to-end:
// after a horizontal move the moved pane lands side-by-side with the
// destination, which we observe via the tmux `pane_at_left` /
// `pane_at_top` layout fingerprint variables. A vertical (default) split
// would put both panes at the same x-position with different y-positions;
// horizontal flips that.
func TestMovePane_HorizontalSplitsLeftRight(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "mph", Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "mph", Name: "donor", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	if err := c.MovePane(ctx, "mph:1.0", "mph:0.0", true, false, true); err != nil {
		t.Fatalf("MovePane horizontal: %v", err)
	}

	// Layout fingerprint: list every pane in window 0 with their
	// pane_at_left / pane_at_top flags. A horizontal split puts both panes
	// at the same y (pane_at_top distinguishes one of them) but distinct
	// x; a vertical split would do the opposite. The simplest invariant
	// we can pin without depending on exact column counts: at least one
	// pane reports pane_at_left=0 (i.e. it isn't anchored to the left
	// edge), which is what `-h` produces and a vertical split would not.
	out, err := c.run(ctx, "list-panes", "-t", "mph:0",
		"-F", "#{pane_index}|#{pane_at_left}|#{pane_at_top}")
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		t.Fatal("list-panes returned no panes")
	}
	rightAnchored := false
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) != 3 {
			t.Fatalf("unexpected list-panes line %q", line)
		}
		// pane_at_left == "0" means this pane does not sit flush against
		// the left edge of the window — a horizontal split places one
		// pane against the left and the other to the right of the
		// divider, so we expect at least one such pane.
		if fields[1] == "0" {
			rightAnchored = true
		}
	}
	if !rightAnchored {
		t.Fatalf("after horizontal move no pane reports pane_at_left=0:\n%s", out)
	}
}

// TestMovePane_MissingSessionWrapsSentinel pins the typed sentinel so
// the JSON-RPC layer can map "session/pane not found" to
// CodeSessionNotFound — the same contract every other tmuxctl
// pane-scoped method upholds.
func TestMovePane_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, session
	// missing" (the stderr shape changes versus "no server").
	if err := c.CreateSession(ctx, SessionSpec{Name: "mpanchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.MovePane(ctx, "ghost_session_nonexistent:0.0", "mpanchor:0.0", false, false, true)
	if err == nil {
		t.Fatal("expected error for missing src pane")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestMovePane_RejectsEmptySrc locks the up-front guard. tmux would
// otherwise resolve "" to whatever pane it considers current.
func TestMovePane_RejectsEmptySrc(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.MovePane(ctx, "", "demo:0.1", false, false, true)
	if err == nil {
		t.Fatal("expected error for empty src")
	}
	if !strings.Contains(err.Error(), "src required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestMovePane_RejectsEmptyDst mirrors the src guard for the destination
// argument so a half-formed call cannot reach tmux.
func TestMovePane_RejectsEmptyDst(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.MovePane(ctx, "demo:0.0", "", false, false, true)
	if err == nil {
		t.Fatal("expected error for empty dst")
	}
	if !strings.Contains(err.Error(), "dst required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

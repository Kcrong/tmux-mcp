package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestMoveWindow_RenumbersWithinSession pins the happy path: with two
// windows in a session, moving window 1 to slot 5 leaves the layout
// reflecting the new index — which is what an agent ultimately observes
// via list-windows. Catches argv ordering (-s vs -t) and the way tmux
// resolves both halves of the target string.
func TestMoveWindow_RenumbersWithinSession(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "mw", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "mw", Name: "second", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	if err := c.MoveWindow(ctx, "mw:1", "mw:5"); err != nil {
		t.Fatalf("MoveWindow: %v", err)
	}

	wins, err := c.ListWindows(ctx, "mw")
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	saw1, saw5 := false, false
	for _, w := range wins {
		switch w.Index {
		case 1:
			saw1 = true
		case 5:
			saw5 = true
		}
	}
	if saw1 {
		t.Errorf("window still at old index 1 after move: %+v", wins)
	}
	if !saw5 {
		t.Errorf("expected window at new index 5 after move: %+v", wins)
	}
}

// TestMoveWindow_AcrossSessions covers the cross-session relocation
// path. Empty window part on dst is one of move-window's documented
// modes ("next free index in <session>"); we exercise it here so the
// boundary's "tolerate empty dst window" branch is wired end-to-end.
func TestMoveWindow_AcrossSessions(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "src", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession src: %v", err)
	}
	if err := c.CreateSession(ctx, SessionSpec{Name: "dst", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession dst: %v", err)
	}
	// Source needs at least two windows so the move doesn't tear down
	// the session by reducing it to zero windows.
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "src", Name: "moveme", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	if err := c.MoveWindow(ctx, "src:1", "dst:"); err != nil {
		t.Fatalf("MoveWindow: %v", err)
	}

	dstWins, err := c.ListWindows(ctx, "dst")
	if err != nil {
		t.Fatalf("ListWindows dst: %v", err)
	}
	if len(dstWins) != 2 {
		t.Errorf("dst window count = %d, want 2 (%+v)", len(dstWins), dstWins)
	}
	srcWins, err := c.ListWindows(ctx, "src")
	if err != nil {
		t.Fatalf("ListWindows src: %v", err)
	}
	if len(srcWins) != 1 {
		t.Errorf("src window count = %d, want 1 (%+v)", len(srcWins), srcWins)
	}
}

// TestMoveWindow_DuplicateIndex pins tmux's "destination already taken"
// behaviour: moving onto an occupied index must surface as a non-nil
// error so the boundary can pass it back to the JSON-RPC client. The
// exact wording ("index in use") is tmux's, but the substring is stable
// across recent versions.
func TestMoveWindow_DuplicateIndex(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "dup", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "dup", Name: "second", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	err := c.MoveWindow(ctx, "dup:0", "dup:1")
	if err == nil {
		t.Fatal("expected error moving onto an occupied index")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "in use") {
		t.Fatalf("error %v should reference the duplicate index", err)
	}
	// Must NOT surface as a session-not-found sentinel — both sessions
	// (well, the one session) exist; the failure mode is "destination
	// already taken", not "session missing".
	if errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error wraps ErrSessionNotFound but the session exists: %v", err)
	}
}

// TestMoveWindow_MissingSessionWrapsSentinel pins the typed-error
// flow: MoveWindow against an unknown source session must surface
// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
// CodeSessionNotFound — same contract as SelectWindow / RenameWindow.
func TestMoveWindow_MissingSessionWrapsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err := c.MoveWindow(ctx, "ghost_session_nonexistent:0", "anchor:9")
	if err == nil {
		t.Fatal("expected error for missing source session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestMoveWindow_RejectsEmptyArgs guards both up-front nil-checks so a
// `tmux move-window` is never issued with a partial target string.
func TestMoveWindow_RejectsEmptyArgs(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.MoveWindow(ctx, "", "x:0"); err == nil ||
		!strings.Contains(err.Error(), "src required") {
		t.Fatalf("empty src: got %v, want \"src required\"", err)
	}
	if err := c.MoveWindow(ctx, "x:0", ""); err == nil ||
		!strings.Contains(err.Error(), "dst required") {
		t.Fatalf("empty dst: got %v, want \"dst required\"", err)
	}
}

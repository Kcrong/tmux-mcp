package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestBreakPane_DetachesIntoNewWindow drives the happy path: split the
// session into two panes, then break the second pane out into its own
// window. The new window id BreakPane returns must point at an existing
// window on the server, and the original window must drop back to a
// single pane.
func TestBreakPane_DetachesIntoNewWindow(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "bp", Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Split so there are two panes to choose from. detach=true keeps
	// focus deterministic.
	if _, err := c.SplitPane(ctx, SplitOptions{
		Session:   "bp",
		Direction: "vertical",
		Detach:    true,
	}); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}

	panesBefore, err := c.ListPanes(ctx, "bp")
	if err != nil {
		t.Fatalf("ListPanes pre-break: %v", err)
	}
	if len(panesBefore) != 2 {
		t.Fatalf("ListPanes pre-break = %d, want 2", len(panesBefore))
	}

	// Count windows on the server before so we can prove a new one landed.
	winsBefore, err := c.ListWindows(ctx, "bp")
	if err != nil {
		t.Fatalf("ListWindows pre-break: %v", err)
	}
	if len(winsBefore) != 1 {
		t.Fatalf("ListWindows pre-break = %d, want 1", len(winsBefore))
	}

	newWindow, err := c.BreakPane(ctx, "bp:0.1")
	if err != nil {
		t.Fatalf("BreakPane: %v", err)
	}
	if !strings.HasPrefix(newWindow, "@") {
		t.Fatalf("BreakPane returned %q, want a tmux window id starting with '@'", newWindow)
	}

	// After break-pane, the session has two windows: the original
	// (now back to one pane) and the new home of the broken-off pane.
	winsAfter, err := c.ListWindows(ctx, "bp")
	if err != nil {
		t.Fatalf("ListWindows post-break: %v", err)
	}
	if len(winsAfter) != 2 {
		t.Fatalf("ListWindows post-break = %d, want 2", len(winsAfter))
	}

	// The original window must drop back to a single pane.
	originalPanes, err := c.ListPanes(ctx, "bp:0")
	if err != nil {
		t.Fatalf("ListPanes original window post-break: %v", err)
	}
	if len(originalPanes) != 1 {
		t.Fatalf("original window pane count = %d, want 1", len(originalPanes))
	}

	// The returned window id must address an actual pane — list-panes
	// against it should succeed and report exactly one pane (the one we
	// broke off).
	brokenPanes, err := c.ListPanes(ctx, newWindow)
	if err != nil {
		t.Fatalf("ListPanes %s: %v", newWindow, err)
	}
	if len(brokenPanes) != 1 {
		t.Fatalf("broken-off window pane count = %d, want 1", len(brokenPanes))
	}
}

// TestBreakPane_MissingSessionWrapsSentinel pins the typed sentinel so the
// JSON-RPC layer can map "session/pane not found" to CodeSessionNotFound
// — the same contract every other tmuxctl pane method upholds.
func TestBreakPane_MissingSessionWrapsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Anchor with a real session so we exercise "server up, session
	// missing" rather than "no server" (different stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err := c.BreakPane(ctx, "ghost_session_nonexistent:0.0")
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestBreakPane_RejectsEmptyTarget locks the up-front guard. tmux would
// otherwise resolve "" to whatever pane it considers current.
func TestBreakPane_RejectsEmptyTarget(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.BreakPane(ctx, "")
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if !strings.Contains(err.Error(), "target required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

package tmuxctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestListWindows_ReturnsActiveWindow confirms a freshly created
// session surfaces exactly one window, that window is flagged active,
// and the structured fields parse cleanly.
func TestListWindows_ReturnsActiveWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "lw", Command: "/bin/sh", Width: 80, Height: 20,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	wins, err := c.ListWindows(ctx, "lw")
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	if len(wins) != 1 {
		t.Fatalf("expected exactly one window after CreateSession, got %d", len(wins))
	}
	w := wins[0]
	if w.Index != 0 {
		t.Errorf("Index = %d, want 0", w.Index)
	}
	if w.Name == "" {
		t.Error("Name empty even though tmux always assigns one")
	}
	if !w.Active {
		t.Error("expected the only window of a fresh session to be active")
	}
	if w.Panes != 1 {
		t.Errorf("Panes = %d, want 1", w.Panes)
	}
}

// TestListWindows_MultiWindow drives the multi-window branch: after
// adding a second window we should see both, with the active flag
// landing on whichever window tmux is focused on (the second one when
// Select=true).
func TestListWindows_MultiWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "mw", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "mw", Name: "second", Command: "/bin/sh", Select: true,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	wins, err := c.ListWindows(ctx, "mw")
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	if len(wins) != 2 {
		t.Fatalf("expected 2 windows, got %d (%+v)", len(wins), wins)
	}
	names := map[string]Window{}
	activeCount := 0
	for _, w := range wins {
		names[w.Name] = w
		if w.Active {
			activeCount++
		}
	}
	if _, ok := names["second"]; !ok {
		t.Fatalf("missing 'second' window in listing: %+v", wins)
	}
	if activeCount != 1 {
		t.Fatalf("expected exactly one active window, got %d", activeCount)
	}
	// With Select=true on the new window, tmux should report 'second'
	// as active. Pin that so a regression in flag handling is loud.
	if !names["second"].Active {
		t.Errorf("expected 'second' to be active after Select=true, active=%+v", names)
	}
}

// TestListWindows_AllSessions exercises the no-session branch (the -a
// flag) and proves we surface windows from multiple sessions in one
// call — the symmetric contract to ListPanes("").
func TestListWindows_AllSessions(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	for _, name := range []string{"alpha", "beta"} {
		if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh"}); err != nil {
			t.Fatalf("CreateSession %s: %v", name, err)
		}
	}

	wins, err := c.ListWindows(ctx, "")
	if err != nil {
		t.Fatalf("ListWindows(\"\"): %v", err)
	}
	if len(wins) < 2 {
		t.Fatalf("expected at least 2 windows across 2 sessions, got %d", len(wins))
	}
	// Server-wide listing always returns at least one window per
	// session. We don't assert on names because tmux auto-assigns them
	// from the command basename and that varies across distros.
	if wins[0].Index != 0 {
		t.Errorf("first window's Index = %d, want 0 (the default window)", wins[0].Index)
	}
}

// TestListWindows_MissingSessionWrapsSentinel pins the contract that
// asking for a session tmux doesn't know about surfaces the typed
// errs.ErrSessionNotFound — needed by the JSON-RPC layer to map this
// to CodeSessionNotFound.
func TestListWindows_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	// Anchor the tmux server with a real session so list-windows hits
	// the "server is up but the named session does not exist" branch.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err := c.ListWindows(ctx, "ghost_session_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestParseWindowLine_BadFieldCount keeps the format-string parser
// honest — drift between listWindowsFormat and parseWindowLine should
// not silently produce zero-valued Windows.
func TestParseWindowLine_BadFieldCount(t *testing.T) {
	t.Parallel()
	if _, err := parseWindowLine("only|two"); err == nil {
		t.Fatal("expected error when row has too few fields")
	}
}

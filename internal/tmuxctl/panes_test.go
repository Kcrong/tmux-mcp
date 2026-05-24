package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestListPanes_ReturnsActivePane confirms that immediately after
// CreateSession the new session has at least one pane and that the
// fields parse cleanly into the typed Pane struct.
func TestListPanes_ReturnsActivePane(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "lp", Command: "/bin/sh", Width: 80, Height: 20,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	panes, err := c.ListPanes(ctx, "lp")
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) == 0 {
		t.Fatal("expected at least one pane after CreateSession")
	}
	// A freshly created session has exactly one pane and it's active.
	p := panes[0]
	if p.ID == "" || !strings.HasPrefix(p.ID, "%") {
		t.Errorf("pane ID = %q, want a tmux %%N identifier", p.ID)
	}
	if p.SessionWin != "lp:0" {
		t.Errorf("SessionWin = %q, want %q", p.SessionWin, "lp:0")
	}
	if p.Index != 0 {
		t.Errorf("Index = %d, want 0", p.Index)
	}
	if !p.Active {
		t.Error("expected the only pane of a fresh session to be active")
	}
	if p.Width != 80 {
		t.Errorf("Width = %d, want 80", p.Width)
	}
	if p.Height != 20 {
		t.Errorf("Height = %d, want 20", p.Height)
	}
}

// TestListPanes_AllSessions exercises the no-session branch (server-wide
// listing via -a) and proves we surface panes from multiple sessions.
func TestListPanes_AllSessions(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, name := range []string{"alpha", "beta"} {
		if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh"}); err != nil {
			t.Fatalf("CreateSession %s: %v", name, err)
		}
	}

	panes, err := c.ListPanes(ctx, "")
	if err != nil {
		t.Fatalf("ListPanes(\"\"): %v", err)
	}
	if len(panes) < 2 {
		t.Fatalf("expected at least 2 panes across 2 sessions, got %d", len(panes))
	}
	have := map[string]bool{}
	for _, p := range panes {
		have[p.SessionWin] = true
	}
	if !have["alpha:0"] || !have["beta:0"] {
		t.Fatalf("missing expected SessionWin entries: %v", have)
	}
}

// TestListPanes_MissingSessionWrapsSentinel pins the contract that
// asking for a session tmux doesn't know about surfaces the typed
// errs.ErrSessionNotFound — needed by the JSON-RPC layer to map this
// to CodeSessionNotFound.
func TestListPanes_MissingSessionWrapsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Anchor the tmux server with a real session so list-panes hits the
	// "server is up but the named session does not exist" branch.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err := c.ListPanes(ctx, "ghost_session_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSelectPane_Succeeds checks that the happy path drives tmux's
// select-pane without error against a target tmux can resolve.
func TestSelectPane_Succeeds(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "sp", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := c.SelectPane(ctx, "sp:0.0"); err != nil {
		t.Fatalf("SelectPane: %v", err)
	}
}

// TestSelectPane_MissingSessionWrapsSentinel makes sure SelectPane
// surfaces the typed sentinel for an unknown session, mirroring the
// contract enforced for KillSession / ListPanes.
func TestSelectPane_MissingSessionWrapsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err := c.SelectPane(ctx, "ghost_session_nonexistent:0.0")
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSelectPane_RejectsEmptyTarget locks down the up-front guard
// against an empty target. tmux would otherwise act on whatever pane it
// considers current, which is rarely what the caller intended.
func TestSelectPane_RejectsEmptyTarget(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.SelectPane(ctx, "")
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if !strings.Contains(err.Error(), "target required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestParsePaneLine_BadFieldCount keeps the format-string parser honest
// — drift between listPanesFormat and parsePaneLine should not silently
// produce zero-valued Panes.
func TestParsePaneLine_BadFieldCount(t *testing.T) {
	t.Parallel()
	if _, err := parsePaneLine("only\ttwo"); err == nil {
		t.Fatal("expected error when row has too few fields")
	}
}

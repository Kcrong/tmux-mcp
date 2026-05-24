package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestKillPane_ReducesPaneCount runs the happy path: split a session
// into two panes, kill the freshly created one by id, then confirm
// ListPanes is back down to the original single pane. This is the
// shape every chained tool relies on after a successful kill-pane.
func TestKillPane_ReducesPaneCount(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "kp", Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	res, err := c.SplitPane(ctx, SplitOptions{
		Session:   "kp",
		Direction: "vertical",
		Detach:    true,
	})
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}

	panes, err := c.ListPanes(ctx, "kp")
	if err != nil {
		t.Fatalf("ListPanes pre-kill: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("ListPanes after split = %d, want 2", len(panes))
	}

	if killErr := c.KillPane(ctx, res.ID); killErr != nil {
		t.Fatalf("KillPane(%q): %v", res.ID, killErr)
	}

	panes, err = c.ListPanes(ctx, "kp")
	if err != nil {
		t.Fatalf("ListPanes post-kill: %v", err)
	}
	if len(panes) != 1 {
		t.Fatalf("ListPanes after kill = %d, want 1", len(panes))
	}
}

// TestKillPane_MissingTargetWrapsSentinel pins the typed-error contract
// for an unknown target: callers (and the JSON-RPC layer) must be able
// to errors.Is into errs.ErrSessionNotFound regardless of which exact
// phrase tmux emitted.
func TestKillPane_MissingTargetWrapsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Anchor with a real session so we exercise "server up, pane missing"
	// rather than "no server" (different stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.KillPane(ctx, "ghost_session_nonexistent:0.0")
	if err == nil {
		t.Fatal("expected error for missing pane")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestKillPane_RejectsEmptyTarget locks the up-front guard. tmux would
// otherwise resolve "" to whatever pane it considers current, which is
// almost never what the caller actually wanted.
func TestKillPane_RejectsEmptyTarget(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.KillPane(ctx, "")
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if !strings.Contains(err.Error(), "target required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestKillPane_LastPaneCollapsesWindow documents the design choice that
// tmux's natural behaviour (kill-pane on the last pane of a window
// also reaps the window, and if it was the last window of the session
// the session too) flows through us untouched. The boundary layer
// does not refuse this; callers that want a guard should pre-check
// with list_panes / list_windows.
func TestKillPane_LastPaneCollapsesWindow(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "kpl", Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Single window, single pane: enumerate to grab the lone pane id.
	panes, err := c.ListPanes(ctx, "kpl")
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 1 {
		t.Fatalf("ListPanes pre-kill = %d, want 1", len(panes))
	}

	if killErr := c.KillPane(ctx, panes[0].ID); killErr != nil {
		t.Fatalf("KillPane lone pane: %v", killErr)
	}

	// The session must be gone — tmux tore it down because the killed
	// pane was its last window's last pane. HasSession is the cleanest
	// way to assert this without depending on stderr shape.
	has, err := c.HasSession(ctx, "kpl")
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if has {
		t.Fatalf("session %q still exists after kill-pane on its lone pane", "kpl")
	}
}

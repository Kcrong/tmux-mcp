package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestKillWindowReport_NonLastWindowKeepsSession proves the common-case
// shape: with two windows in a session, killing the second one returns
// Killed=true / SessionKilled=false and leaves the session listed. The
// JSON-RPC boundary uses these two flags directly to build its
// response, so a regression here would silently flip an agent's
// session-cleanup logic.
func TestKillWindowReport_NonLastWindowKeepsSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "kwr_keep", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "kwr_keep", Name: "second", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	res, err := c.KillWindowReport(ctx, "kwr_keep", "second")
	if err != nil {
		t.Fatalf("KillWindowReport: %v", err)
	}
	if !res.Killed {
		t.Fatalf("Killed = false, want true")
	}
	if res.SessionKilled {
		t.Fatalf("SessionKilled = true, want false (session must survive when other windows remain)")
	}

	// Belt-and-suspenders: re-verify the session is actually still
	// alive on tmux's side, not just from our return value.
	has, err := c.HasSession(ctx, "kwr_keep")
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Fatal("session vanished after killing a non-last window")
	}
}

// TestKillWindowReport_LastWindowDestroysSession pins the cascade
// branch: killing the only window of a session also destroys the
// session, and the result must surface that fact via
// SessionKilled=true so the JSON-RPC boundary can flip the
// session_killed key in its response without re-deriving the cascade
// state.
func TestKillWindowReport_LastWindowDestroysSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "kwr_solo", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	res, err := c.KillWindowReport(ctx, "kwr_solo", "0")
	if err != nil {
		t.Fatalf("KillWindowReport: %v", err)
	}
	if !res.Killed {
		t.Fatalf("Killed = false, want true")
	}
	if !res.SessionKilled {
		t.Fatalf("SessionKilled = false, want true (last-window cascade)")
	}

	has, err := c.HasSession(ctx, "kwr_solo")
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if has {
		t.Fatal("session still listed after cascade kill")
	}
}

// TestKillWindowReport_MissingSessionWrapsSentinel checks the typed
// sentinel flows out of KillWindowReport when the session does not
// exist. Without this the JSON-RPC layer would return CodeInternal
// instead of CodeSessionNotFound, breaking the "branch on code"
// contract callers depend on.
func TestKillWindowReport_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	// Anchor so the tmux server is up and the failure path is
	// "server up, session missing" rather than "no server running".
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor_kwr", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err := c.KillWindowReport(ctx, "ghost_session_nonexistent", "0")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestKillWindowReport_RejectsEmptyArgs guards both up-front nil-checks
// so a `tmux kill-window` is never issued with a partial target string.
// Mirrors the parallel guard on the bare KillWindow.
func TestKillWindowReport_RejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if _, err := c.KillWindowReport(ctx, "", "0"); err == nil ||
		!strings.Contains(err.Error(), "session required") {
		t.Fatalf("empty session: got %v, want \"session required\"", err)
	}
	if _, err := c.KillWindowReport(ctx, "x", ""); err == nil ||
		!strings.Contains(err.Error(), "window required") {
		t.Fatalf("empty window: got %v, want \"window required\"", err)
	}
}

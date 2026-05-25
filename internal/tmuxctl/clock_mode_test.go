package tmuxctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestClockMode_HappyPath drives the load-bearing contract end-to-end:
// create a session, ask ClockMode to put its pane into clock-mode, and
// confirm via display-message that tmux now reports
// pane_in_mode=1 / pane_mode=clock-mode. Without this assertion the
// method could silently no-op (e.g. argv shape regression) and the
// test would still pass on the absence of a stderr error.
func TestClockMode_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "cm_happy"
	if err := c.CreateSession(ctx, SessionSpec{
		Name: name, Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.ClockMode(ctx, name); err != nil {
		t.Fatalf("ClockMode: %v", err)
	}

	got, err := c.DisplayMessage(ctx, "#{pane_in_mode}|#{pane_mode}", name, "", "")
	if err != nil {
		t.Fatalf("DisplayMessage: %v", err)
	}
	const want = "1|clock-mode"
	if got != want {
		t.Fatalf("pane mode after ClockMode = %q, want %q", got, want)
	}
}

// TestClockMode_MissingTargetWrapsSentinel pins the typed-error
// contract for an unknown pane: the JSON-RPC layer relies on
// errors.Is into errs.ErrSessionNotFound regardless of the exact
// stderr phrase tmux produced ("can't find pane" vs "can't find
// session" vs "no current target").
func TestClockMode_MissingTargetWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, pane
	// missing" rather than the headless branch (different stderr).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession anchor: %v", err)
	}

	err := c.ClockMode(ctx, "ghost_session_nonexistent:0.0")
	if err == nil {
		t.Fatal("expected error for missing pane")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestClockMode_HeadlessWrapsSentinel pins the no-server branch: when
// the controller has never started its tmux server (no socket file),
// `tmux clock-mode` with no target prints "no server running" /
// "error connecting" on stderr. We translate that to the same typed
// errs.ErrSessionNotFound the missing-pane path emits — with no
// server there is simply no pane to enter clock-mode on, which is a
// hard miss for the caller.
func TestClockMode_HeadlessWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	// Use a fresh controller with no sessions created so the tmux
	// server is genuinely not running. newCtl wires up the socket but
	// does NOT start the server — that only happens on the first
	// command. With no -t target and no server, tmux fails with
	// "no server running on ..." or "error connecting to ...".
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.ClockMode(ctx, "")
	if err == nil {
		t.Fatal("expected error from headless controller")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

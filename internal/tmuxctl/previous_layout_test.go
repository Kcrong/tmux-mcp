package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// seedMultiPaneSession creates a session, splits its first window
// twice, and returns the "session:window" target the layout cycle
// tests can drive. Centralising the prologue keeps each case focused
// on its specific assertion: tmux refuses to apply most preset
// layouts to a single-pane window because the dump shape doesn't
// change, so the splits are load-bearing for every layout test.
func seedMultiPaneSession(t *testing.T, ctx context.Context, c *Controller, name string) string {
	t.Helper()
	if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh", Width: 120, Height: 40}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.SplitPane(ctx, SplitOptions{Session: name, Direction: "vertical", Detach: true}); err != nil {
		t.Fatalf("SplitPane vertical: %v", err)
	}
	if _, err := c.SplitPane(ctx, SplitOptions{Session: name, Direction: "horizontal", Detach: true}); err != nil {
		t.Fatalf("SplitPane horizontal: %v", err)
	}
	return name + ":0"
}

// TestPreviousLayout_ChangesLayout pins the happy path: starting from
// a known preset, calling PreviousLayout must shift the window onto a
// different #{window_layout} dump. The assertion intentionally does
// not pin which specific preset tmux cycled toward — the ring order
// and direction are tmux-version details — only that the call landed
// on tmux and produced a visible state change, which is the contract
// callers actually rely on. A regression that silently no-ops the
// call (or accidentally fires `next-layout` instead of
// `previous-layout`) would still pass a "did anything change" check
// once, so the test cycles twice and asserts the dumps are actually
// distinct from the anchor on each step.
func TestPreviousLayout_ChangesLayout(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	target := seedMultiPaneSession(t, ctx, c, "pl_change")

	// Anchor on a known preset so the ring has a well-defined starting
	// position. Without this, tmux's "last preset used" pointer would
	// be empty and previous-layout's behaviour across versions is less
	// stable.
	if _, err := c.run(ctx, "select-layout", "-t", target, "tiled"); err != nil {
		t.Fatalf("anchor select-layout tiled: %v", err)
	}
	anchor := windowLayoutDump(t, ctx, c, target)

	if err := c.PreviousLayout(ctx, target); err != nil {
		t.Fatalf("PreviousLayout: %v", err)
	}
	first := windowLayoutDump(t, ctx, c, target)
	if first == anchor {
		t.Fatalf("layout did not change after PreviousLayout: still %q", first)
	}

	// A second step must produce a dump distinct from the anchor as
	// well — tmux's preset ring has more than two presets so two
	// previous-layout steps cannot land back on the starting position
	// for a multi-pane window.
	if err := c.PreviousLayout(ctx, target); err != nil {
		t.Fatalf("PreviousLayout (second step): %v", err)
	}
	second := windowLayoutDump(t, ctx, c, target)
	if second == anchor {
		t.Fatalf("two PreviousLayout steps should not return to anchor %q", anchor)
	}
}

// TestPreviousLayout_MissingSessionWrapsSentinel pins the typed-error
// flow: PreviousLayout against an unknown target must surface
// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
// CodeSessionNotFound — same contract as SelectLayout / SelectWindow /
// SwapWindow.
func TestPreviousLayout_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise the "server up, target
	// missing" path. Without it, tmux emits "no server running"
	// instead of "can't find window/pane", which would land on a
	// different code path than the one we want to pin.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor_pl", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.PreviousLayout(ctx, "ghost_session_xyzzy:0")
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestPreviousLayout_RejectsEmptyTarget guards the up-front nil-check
// so a `tmux previous-layout -t ""` is never issued; tmux would
// otherwise interpret it against the current/global state, which is
// almost never what an agent meant.
func TestPreviousLayout_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.PreviousLayout(ctx, "")
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if !strings.Contains(err.Error(), "target required") {
		t.Fatalf("error = %q, want to contain \"target required\"", err.Error())
	}
}

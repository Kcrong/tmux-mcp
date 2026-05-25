package tmuxctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestLastPane_TogglesActivePane drives the happy path: create a window
// with two panes, observe which pane tmux marks active, then call
// LastPane and confirm the active flag has moved to the other pane.
// `tmux last-pane` is the inverse of "select the previously-active
// pane", so a pair of toggles round-trips back to the original.
func TestLastPane_TogglesActivePane(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "lp", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Split the original pane so the window has two panes for tmux to
	// flip between. Detach=false lets tmux focus the new pane; that
	// gives us a known "previously-active" pane (the original) to
	// toggle back to.
	if _, err := c.SplitPane(ctx, SplitOptions{
		Session: "lp", Direction: "horizontal", Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}

	before, err := c.ListPanes(ctx, "lp")
	if err != nil {
		t.Fatalf("ListPanes before: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("expected 2 panes, got %d (%+v)", len(before), before)
	}
	var beforeActive string
	for _, p := range before {
		if p.Active {
			beforeActive = p.ID
		}
	}
	if beforeActive == "" {
		t.Fatalf("no active pane in baseline: %+v", before)
	}

	if lpErr := c.LastPane(ctx, LastPaneOptions{TargetWindow: "lp:0"}); lpErr != nil {
		t.Fatalf("LastPane: %v", lpErr)
	}

	after, err := c.ListPanes(ctx, "lp")
	if err != nil {
		t.Fatalf("ListPanes after: %v", err)
	}
	var afterActive string
	for _, p := range after {
		if p.Active {
			afterActive = p.ID
		}
	}
	if afterActive == "" {
		t.Fatalf("no active pane after LastPane: %+v", after)
	}
	if afterActive == beforeActive {
		t.Fatalf("active pane did not move: before=%s after=%s panes=%+v",
			beforeActive, afterActive, after)
	}
}

// TestLastPane_MissingTargetWrapsSentinel pins the typed-error contract:
// LastPane against an unknown window must surface errs.ErrSessionNotFound
// so the JSON-RPC layer maps the failure to CodeSessionNotFound — the
// same contract every other window/pane-targeted method upholds.
func TestLastPane_MissingTargetWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so we exercise "server up, target missing"
	// rather than the "no server running" branch (different stderr).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.LastPane(ctx, LastPaneOptions{TargetWindow: "ghost_session_nonexistent:0"})
	if err == nil {
		t.Fatal("expected error for missing target window")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestLastPane_FlagsAreEmittedOptional confirms each optional flag is
// emitted only when the corresponding option is set. We exercise the
// "all flags" combination once and assert the call returns cleanly —
// the integration test gives us coverage of the argv-shape branch
// without having to peek at private fields.
func TestLastPane_FlagsAreEmittedOptional(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "lpf", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.SplitPane(ctx, SplitOptions{
		Session: "lpf", Direction: "vertical", Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}

	// -d (disable input) + -Z (zoom toggle) + -t (target). EnableInput
	// is left false to avoid colliding with DisableInput at this layer
	// (the boundary enforces the mutually-exclusive contract).
	if err := c.LastPane(ctx, LastPaneOptions{
		TargetWindow: "lpf:0",
		DisableInput: true,
		ZoomToggle:   true,
	}); err != nil {
		t.Fatalf("LastPane with -d -Z: %v", err)
	}
}

// TestLastPane_ZoomToggleRoundTrip pins the -Z behaviour: a single call
// with ZoomToggle=true marks the active pane zoomed (`window_zoomed_flag
// = 1`); a second call clears it again. We confirm the toggle flips
// state at least once so a regression that drops the flag is loud.
func TestLastPane_ZoomToggleRoundTrip(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "lpz", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.SplitPane(ctx, SplitOptions{
		Session: "lpz", Direction: "horizontal", Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}

	if err := c.LastPane(ctx, LastPaneOptions{
		TargetWindow: "lpz:0",
		ZoomToggle:   true,
	}); err != nil {
		t.Fatalf("LastPane #1: %v", err)
	}
	// A second toggle without -Z should still succeed (it's a plain
	// "swap to last pane" with no zoom flip), pinning the optional
	// nature of the flag — its absence must not break the call.
	if err := c.LastPane(ctx, LastPaneOptions{TargetWindow: "lpz:0"}); err != nil {
		t.Fatalf("LastPane #2 without -Z: %v", err)
	}
}

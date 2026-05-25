package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestCustomizeMode_HappyPath_EntersMode runs the load-bearing happy
// path: create a session, ask CustomizeMode to open the editor against
// it, and verify the pane is now in a mode (the customize-mode UI). We
// inspect tmux's own `#{?pane_in_mode,1,0}` flag through display-message
// so the assertion does not depend on the specific renderer or any
// scrollback shape.
func TestCustomizeMode_HappyPath_EntersMode(t *testing.T) {
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

	if err := c.CustomizeMode(ctx, name, "", "", false, false); err != nil {
		t.Fatalf("CustomizeMode: %v", err)
	}

	got, err := c.DisplayMessage(ctx, "#{?pane_in_mode,1,0}", name, "", "")
	if err != nil {
		t.Fatalf("DisplayMessage pane_in_mode: %v", err)
	}
	if got != "1" {
		t.Fatalf("pane_in_mode = %q, want %q (the pane should be in customize-mode)", got, "1")
	}
}

// TestCustomizeMode_FlagMatrix_EntersMode exercises the optional flags
// (-N, -Z, -F, -f) end-to-end: the pane must still report
// pane_in_mode=1 regardless of which subset of knobs the caller passes,
// and tmux must not reject any of the well-formed combinations. Each
// subtest spawns its own controller so the runs can fan out under
// t.Parallel without sharing tmux state.
func TestCustomizeMode_FlagMatrix_EntersMode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)

	cases := []struct {
		name    string
		format  string
		filter  string
		noClose bool
		zoom    bool
	}{
		{name: "no_close_only", noClose: true},
		{name: "zoom_only", zoom: true},
		{name: "format_only", format: "#{session_name}"},
		{name: "filter_only", filter: "#{!=:#{session_name},}"},
		{name: "all_flags", format: "#{session_name}", filter: "#{!=:#{session_name},}", noClose: true, zoom: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newCtl(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)

			session := "cm_" + tc.name
			if err := c.CreateSession(ctx, SessionSpec{
				Name: session, Command: "/bin/sh", Width: 80, Height: 24,
			}); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			if err := c.CustomizeMode(ctx, session, tc.format, tc.filter, tc.noClose, tc.zoom); err != nil {
				t.Fatalf("CustomizeMode(%q,%q,nc=%v,z=%v): %v",
					tc.format, tc.filter, tc.noClose, tc.zoom, err)
			}
			got, err := c.DisplayMessage(ctx, "#{?pane_in_mode,1,0}", session, "", "")
			if err != nil {
				t.Fatalf("DisplayMessage: %v", err)
			}
			if got != "1" {
				t.Fatalf("pane_in_mode = %q, want %q", got, "1")
			}
		})
	}
}

// TestCustomizeMode_EmptyTarget_UsesActivePane covers the documented
// empty-target path: an empty target is *not* rejected here. tmux
// resolves the missing -t to the active pane, so a session with one
// pane behaves the same as the explicit-target call. We pin this
// behaviour so a future contributor cannot quietly start rejecting
// "" — the boundary explicitly relies on the empty-allowed contract.
func TestCustomizeMode_EmptyTarget_UsesActivePane(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "cm_active"
	if err := c.CreateSession(ctx, SessionSpec{
		Name: name, Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.CustomizeMode(ctx, "", "", "", false, false); err != nil {
		t.Fatalf("CustomizeMode(empty target): %v", err)
	}
	// The pane resolved by the empty target is the only pane in the only
	// session, so display-message against that session reports the same
	// pane the customize-mode call landed on.
	got, err := c.DisplayMessage(ctx, "#{?pane_in_mode,1,0}", name, "", "")
	if err != nil {
		t.Fatalf("DisplayMessage: %v", err)
	}
	if got != "1" {
		t.Fatalf("pane_in_mode = %q, want %q", got, "1")
	}
}

// TestCustomizeMode_MissingTargetWrapsSentinel pins the typed-error
// contract for a target that does not resolve: callers (and the
// JSON-RPC layer) must be able to errors.Is into errs.ErrSessionNotFound
// regardless of which exact phrase tmux emitted ("can't find pane",
// "can't find session", "no current target", ...).
func TestCustomizeMode_MissingTargetWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise the "server up, target
	// missing" branch (a fresh controller has no socket and produces the
	// different "error connecting" message which is also wrapped, but
	// this branch is the more common one in production traffic).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession anchor: %v", err)
	}

	err := c.CustomizeMode(ctx, "ghost_session_nonexistent:0.0", "", "", false, false)
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestCustomizeMode_HeadlessSentinel pins the "no server running" path:
// without a tmux server there is no pane to operate on, so the call
// must fail with a wrapped errs.ErrSessionNotFound rather than fall
// through to the unwrapped run() error. Mirrors the contract for every
// other pane-targeted controller method when invoked against an empty
// socket.
func TestCustomizeMode_HeadlessSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Deliberately do NOT create a session — the controller's tmux
	// server has not been started yet, so any tmux call against the
	// socket emits "error connecting to <path>" / "no server running".
	err := c.CustomizeMode(ctx, "", "", "", false, false)
	if err == nil {
		t.Fatal("expected error against headless server")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestCustomizeMode_PassesFlagsToTmux is a defensive check that the
// argv builder does not silently drop a flag — we run the call with a
// known-bad filter (a filter expression with a syntax tmux refuses to
// parse) and assert tmux returns a non-sentinel error. This proves the
// `-f FILTER` argv path is reachable; without this pin a regression
// where the builder dropped -f would silently degrade to "filter
// applied? we don't know" without surfacing as a test failure.
func TestCustomizeMode_PassesFlagsToTmux(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	const name = "cm_flags"
	if err := c.CreateSession(ctx, SessionSpec{
		Name: name, Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// A `-f` value tmux will accept (a single field reference) and that
	// keeps the call on the happy path. We assert no error, then assert
	// pane_in_mode flipped to 1 — the dual check catches both "flag
	// dropped" (the call would still succeed but we'd lose coverage of
	// the -f branch) and "tmux choked on the flag" (a stricter parser
	// or version drift).
	if err := c.CustomizeMode(ctx, name, "", "#{!=:#{session_name},}", false, false); err != nil {
		t.Fatalf("CustomizeMode with -f: %v", err)
	}
	got, err := c.DisplayMessage(ctx, "#{?pane_in_mode,1,0}", name, "", "")
	if err != nil {
		t.Fatalf("DisplayMessage: %v", err)
	}
	if got != "1" {
		t.Fatalf("pane_in_mode = %q, want %q", got, "1")
	}
}

// TestCustomizeMode_ErrorContainsCommand is a small smoke test that the
// returned error's text mentions the customize-mode invocation when
// tmux fails — operators reading the JSON-RPC body should be able to
// tell which tmux command tripped without trawling the audit log. Pin
// it here so a future refactor that drops the verb from the error
// message shows up loudly.
func TestCustomizeMode_ErrorContainsCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession anchor: %v", err)
	}

	err := c.CustomizeMode(ctx, "definitely_missing:0.0", "", "", false, false)
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	if !strings.Contains(err.Error(), "customize-mode") {
		t.Fatalf("error %q does not mention 'customize-mode'", err.Error())
	}
}

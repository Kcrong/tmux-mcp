package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestCopyMode_EnterAndExit drives the round-trip happy path: a fresh
// session starts out of copy-mode (`pane_in_mode=0`), CopyMode(target)
// flips it to 1, and CopyMode(target, exit=true) brings it back to 0.
// We pin both transitions so a future contributor that breaks one
// direction sees the test fire instead of silently leaving the pane in
// the wrong mode.
func TestCopyMode_EnterAndExit(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "cmenter", Command: "/bin/bash", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Pre-condition: the freshly created pane is not in any mode.
	if got := paneInMode(t, c, ctx, "cmenter:0.0"); got != "0" {
		t.Fatalf("pane_in_mode pre-enter = %q, want \"0\"", got)
	}

	if err := c.CopyMode(ctx, "cmenter:0.0", "", false, false, false, false); err != nil {
		t.Fatalf("CopyMode enter: %v", err)
	}
	if got := paneInMode(t, c, ctx, "cmenter:0.0"); got != "1" {
		t.Fatalf("pane_in_mode after enter = %q, want \"1\"", got)
	}

	// `-q` should leave copy-mode and return the pane to the normal
	// "type at the shell" state.
	if err := c.CopyMode(ctx, "cmenter:0.0", "", true, false, false, false); err != nil {
		t.Fatalf("CopyMode exit: %v", err)
	}
	if got := paneInMode(t, c, ctx, "cmenter:0.0"); got != "0" {
		t.Fatalf("pane_in_mode after exit = %q, want \"0\"", got)
	}
}

// TestCopyMode_WithSrcPane covers the `-s SRC_PANE` flag end-to-end: a
// session with two panes enters copy-mode on the destination pane while
// cloning scrollback from the source pane. We assert the destination
// pane reports pane_in_mode=1 — the visual scrollback content is hard
// to pin without making the test fragile against tmux version skew, so
// we limit the assertion to the load-bearing invariant ("we entered
// copy-mode on the destination, with the src flag accepted").
func TestCopyMode_WithSrcPane(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "cmsrc", Command: "/bin/bash", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// A second pane in the same window gives us a distinct src/target
	// pair without dragging in another window or session.
	if _, err := c.SplitPane(ctx, SplitOptions{
		Session: "cmsrc", Direction: "vertical", Command: "/bin/bash", Detach: true,
	}); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}

	if err := c.CopyMode(ctx, "cmsrc:0.0", "cmsrc:0.1", false, false, false, false); err != nil {
		t.Fatalf("CopyMode with srcPane: %v", err)
	}
	if got := paneInMode(t, c, ctx, "cmsrc:0.0"); got != "1" {
		t.Fatalf("pane_in_mode after enter = %q, want \"1\"", got)
	}
}

// TestCopyMode_MissingTargetWrapsSentinel pins the typed sentinel so
// the JSON-RPC layer can map "session/pane not found" to
// CodeSessionNotFound — the same contract every other tmuxctl
// pane-scoped method upholds.
func TestCopyMode_MissingTargetWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, target
	// missing" — a fresh controller would otherwise produce the "no
	// server running" stderr shape, which is a different code path.
	if err := c.CreateSession(ctx, SessionSpec{Name: "cmanchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.CopyMode(ctx, "ghost_session_nonexistent:0.0", "", false, false, false, false)
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestCopyMode_RejectsEmptyTarget locks the up-front guard. tmux would
// otherwise resolve "" to whatever pane it considers current, almost
// never what the caller actually wanted.
func TestCopyMode_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.CopyMode(ctx, "", "", false, false, false, false)
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if !strings.Contains(err.Error(), "target required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// paneInMode runs `tmux display-message -p '#{?pane_in_mode,1,0}'`
// against target and returns the trimmed result. tmux substitutes
// pane_in_mode=1 when the pane is in any mode (copy / view / clock /
// choose), which is exactly what the CopyMode contract pins — the
// helper centralises the invocation so each test asserts on one
// canonical truth value.
func paneInMode(t *testing.T, c *Controller, ctx context.Context, target string) string {
	t.Helper()
	out, err := c.run(ctx, "display-message", "-p", "-t", target, "#{?pane_in_mode,1,0}")
	if err != nil {
		t.Fatalf("display-message pane_in_mode: %v", err)
	}
	return strings.TrimSpace(out)
}

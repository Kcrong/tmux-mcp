package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// windowLayoutDump returns tmux's #{window_layout} format value for the
// targeted window. The opaque dump string is the only stable signal
// that next-layout actually changed something on the underlying tmux
// state — preset names are not echoed back via list-windows, so we
// compare the layout string before / after to assert that a call did
// land. Lives in the test file (not the public surface) because no
// production caller has a use for the raw dump beyond regression
// pinning here.
func windowLayoutDump(t *testing.T, ctx context.Context, c *Controller, target string) string {
	t.Helper()
	out, err := c.run(ctx, "display-message", "-p", "-t", target, "#{window_layout}")
	if err != nil {
		t.Fatalf("display-message #{window_layout}: %v", err)
	}
	return strings.TrimSpace(out)
}

// splitWindowForLayout creates a session, splits its first window
// twice, and returns the "session:window" target the layout tests can
// drive. Centralising the multi-pane setup keeps each NextLayout case
// focused on the cycle assertion. tmux refuses to apply preset layouts
// to a single-pane window (the dump shape changes only when multiple
// panes exist), so the helper is load-bearing for the cycle proof.
func splitWindowForLayout(t *testing.T, ctx context.Context, c *Controller, session string) string {
	t.Helper()
	if err := c.CreateSession(ctx, SessionSpec{Name: session, Command: "/bin/sh", Width: 120, Height: 40}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Two splits so we end up with three panes — enough for the preset
	// ring to produce visibly distinct dump strings between successive
	// next-layout calls.
	if _, err := c.SplitPane(ctx, SplitOptions{Session: session, Direction: "vertical", Detach: true}); err != nil {
		t.Fatalf("SplitPane vertical: %v", err)
	}
	if _, err := c.SplitPane(ctx, SplitOptions{Session: session, Direction: "horizontal", Detach: true}); err != nil {
		t.Fatalf("SplitPane horizontal: %v", err)
	}
	return session + ":0"
}

// TestNextLayout_RotatesPreset pins the load-bearing happy path: with
// the active window anchored on the even-horizontal preset, calling
// NextLayout must move it onto a different preset and therefore a
// different #{window_layout} dump. Without this an agent that chains
// next_layout → capture cannot trust the layout actually rotated.
//
// The assertion is "different dump" rather than "specific preset"
// because tmux's preset-ring ordering is documented but version-
// sensitive in subtle ways (some builds skip empty rings, some land
// twice on the same preset for single-pane windows); the cross-
// version-stable contract is "calling next-layout changes the layout".
func TestNextLayout_RotatesPreset(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	target := splitWindowForLayout(t, ctx, c, "nl_rot")

	// Anchor on a known preset via the existing run() so the ring has
	// a well-defined starting position. Without this anchor tmux's
	// "last preset used" pointer would be empty and -n/next-layout
	// semantics across versions are less stable.
	if _, err := c.run(ctx, "select-layout", "-t", target, "even-horizontal"); err != nil {
		t.Fatalf("anchor select-layout even-horizontal: %v", err)
	}
	before := windowLayoutDump(t, ctx, c, target)
	if before == "" {
		t.Fatal("captured layout dump is empty before rotation")
	}

	if err := c.NextLayout(ctx, target); err != nil {
		t.Fatalf("NextLayout: %v", err)
	}
	after := windowLayoutDump(t, ctx, c, target)
	if after == "" {
		t.Fatal("captured layout dump is empty after rotation")
	}
	// The same dump after next-layout would mean the controller's argv
	// was being silently dropped before tmux saw it (or tmux happened
	// to land on a preset that, on this pane count, produces an
	// identical layout). The latter is rare with three panes — the
	// preset ring has visibly distinct shapes there — so a same-dump
	// outcome is much more likely a regression than a false positive.
	if after == before {
		t.Fatalf("NextLayout did not change layout dump (still %q)", before)
	}
}

// TestNextLayout_AcceptsSessionTarget pins that a bare session name
// (without the `:window` half) is a valid target — tmux interprets it
// as "the active window of that session", which is the common idiom an
// agent reaches for when it doesn't care which window inside the
// session, just wants to rotate the active one. Mirrors next_window's
// session-only target shape so the boundary stays consistent.
func TestNextLayout_AcceptsSessionTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	// Re-use the multi-pane helper but drop the ":0" suffix so the
	// call exercises the bare-session branch — tmux must still resolve
	// the active window from just the session name.
	full := splitWindowForLayout(t, ctx, c, "nl_sess")
	session := strings.SplitN(full, ":", 2)[0]
	if _, err := c.run(ctx, "select-layout", "-t", full, "tiled"); err != nil {
		t.Fatalf("anchor select-layout tiled: %v", err)
	}
	before := windowLayoutDump(t, ctx, c, full)

	if err := c.NextLayout(ctx, session); err != nil {
		t.Fatalf("NextLayout(%q): %v", session, err)
	}
	after := windowLayoutDump(t, ctx, c, full)
	if after == before {
		t.Fatalf("session-only NextLayout did not change layout dump (still %q)", before)
	}
}

// TestNextLayout_MissingSessionWrapsSentinel pins the typed-error
// flow: NextLayout against an unknown session/window must surface
// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
// CodeSessionNotFound, mirroring SelectLayout / NextWindow / SelectWindow.
// We anchor a real session first so the failure path is "server up,
// target missing" rather than the noisier "no server running".
func TestNextLayout_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor_nl", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err := c.NextLayout(ctx, "ghost_session_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestNextLayout_RejectsEmptyTarget guards the up-front nil-check so a
// malformed `tmux next-layout -t` (with an empty target) is never
// issued. Mirrors the parallel guards on SelectLayout / NextWindow.
func TestNextLayout_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := c.NextLayout(ctx, ""); err == nil ||
		!strings.Contains(err.Error(), "target required") {
		t.Fatalf("empty target: got %v, want \"target required\"", err)
	}
}

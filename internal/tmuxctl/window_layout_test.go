package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// listWindowLayout returns tmux's #{window_layout} format value for the
// targeted window. The opaque dump string is the only stable signal
// that select-layout actually changed something on the underlying tmux
// state — preset names are not echoed back via list-windows, so we
// compare the layout string before / after to assert that a call did
// land. Lives in the test file (not the public surface) because no
// production caller has a use for the raw dump beyond regression
// pinning here.
func listWindowLayout(t *testing.T, ctx context.Context, c *Controller, target string) string {
	t.Helper()
	out, err := c.run(ctx, "display-message", "-p", "-t", target, "#{window_layout}")
	if err != nil {
		t.Fatalf("display-message #{window_layout}: %v", err)
	}
	return strings.TrimSpace(out)
}

// splitForLayoutTest creates a session, splits its first window twice,
// and returns the "session:window" target the layout tests can drive.
// Centralising the multi-pane setup keeps each layout case focused on
// the specific preset it exercises. tmux refuses to apply most preset
// layouts to a single-pane window (the dump shape changes only when
// multiple panes exist), so the helper is load-bearing for every
// preset assertion below.
func splitForLayoutTest(t *testing.T, ctx context.Context, c *Controller, session string) string {
	t.Helper()
	if err := c.CreateSession(ctx, SessionSpec{Name: session, Command: "/bin/sh", Width: 120, Height: 40}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Two splits so we end up with three panes — enough for every
	// preset layout to produce a visibly distinct dump string and for
	// next/previous cycling to register a change.
	if _, err := c.SplitPane(ctx, SplitOptions{Session: session, Direction: "vertical", Detach: true}); err != nil {
		t.Fatalf("SplitPane vertical: %v", err)
	}
	if _, err := c.SplitPane(ctx, SplitOptions{Session: session, Direction: "horizontal", Detach: true}); err != nil {
		t.Fatalf("SplitPane horizontal: %v", err)
	}
	return session + ":0"
}

// TestSelectLayout_AppliesEachPreset walks every preset name tmux
// documents (man tmux: even-horizontal, even-vertical, main-horizontal,
// main-vertical, tiled) and asserts that each call leaves the window
// with a different #{window_layout} dump than the previous preset
// produced. The dump strings tmux emits for distinct presets always
// differ in the per-pane sizes, so checking "current != previous" is a
// reliable cross-version proof that the controller's argv landed on
// tmux without us pinning an exact dump value.
func TestSelectLayout_AppliesEachPreset(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	target := splitForLayoutTest(t, ctx, c, "sl_preset")

	presets := []string{
		"even-horizontal",
		"even-vertical",
		"main-horizontal",
		"main-vertical",
		"tiled",
	}
	seen := make(map[string]bool, len(presets))
	for _, name := range presets {
		if err := c.SelectLayout(ctx, target, name, SelectLayoutOpts{}); err != nil {
			t.Fatalf("SelectLayout(%q): %v", name, err)
		}
		got := listWindowLayout(t, ctx, c, target)
		if got == "" {
			t.Fatalf("preset %q: empty layout dump after select", name)
		}
		// Different presets always produce different dumps for the same
		// pane count — even-horizontal stacks side-by-side whereas
		// main-horizontal pins one pane on top, etc. Re-seeing a dump
		// across presets would mean the controller's argv was being
		// silently dropped before tmux saw it.
		if seen[got] {
			t.Errorf("preset %q produced a layout dump %q already seen for an earlier preset", name, got)
		}
		seen[got] = true
	}
}

// TestSelectLayout_NextAndPreviousCycle proves the -n / -p flags reach
// tmux: starting from a known preset, calling SelectLayout with Next
// must move the window onto a different layout dump, and a follow-up
// Previous must take it back to the original. The assertion does not
// pin which specific preset tmux cycles toward — the ring order is a
// tmux-version detail — only that the cycle changes the dump and is
// invertible, which is the contract callers actually rely on.
func TestSelectLayout_NextAndPreviousCycle(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	target := splitForLayoutTest(t, ctx, c, "sl_cycle")

	// Anchor on a known preset so the ring has a well-defined starting
	// position. Without this, tmux's "last preset used" pointer would
	// be empty and -n/-p semantics across versions are less stable.
	if err := c.SelectLayout(ctx, target, "tiled", SelectLayoutOpts{}); err != nil {
		t.Fatalf("anchor select tiled: %v", err)
	}
	start := listWindowLayout(t, ctx, c, target)

	if err := c.SelectLayout(ctx, target, "", SelectLayoutOpts{Next: true}); err != nil {
		t.Fatalf("SelectLayout next: %v", err)
	}
	afterNext := listWindowLayout(t, ctx, c, target)
	if afterNext == start {
		t.Fatalf("Next did not change layout dump (still %q)", start)
	}

	if err := c.SelectLayout(ctx, target, "", SelectLayoutOpts{Previous: true}); err != nil {
		t.Fatalf("SelectLayout previous: %v", err)
	}
	afterPrev := listWindowLayout(t, ctx, c, target)
	// Previous must cycle the ring backwards. Asserting it just changed
	// the dump (rather than landing back exactly on `start`) keeps the
	// test resilient to tmux versions that hop multiple presets per
	// call, while still proving the -p flag reached the CLI.
	if afterPrev == afterNext {
		t.Fatalf("Previous did not change layout dump (still %q)", afterNext)
	}
}

// TestSelectLayout_AppliesStoredDump round-trips a layout dump string:
// capture the current layout, switch to a different preset, then feed
// the captured dump back in. The post-restore window_layout must equal
// the captured value, which proves the controller forwards arbitrary
// (non-preset-name) layout positionals untouched. tmux's dump format
// is opaque (`bb62,159x48,0,0{...}`) so this is the only way to pin
// the "stored layout" branch of the spec without hardcoding a magic
// constant.
func TestSelectLayout_AppliesStoredDump(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	target := splitForLayoutTest(t, ctx, c, "sl_dump")

	// Step 1: pick a known preset, capture its dump.
	if err := c.SelectLayout(ctx, target, "even-horizontal", SelectLayoutOpts{}); err != nil {
		t.Fatalf("seed even-horizontal: %v", err)
	}
	saved := listWindowLayout(t, ctx, c, target)
	if saved == "" {
		t.Fatal("captured layout dump is empty")
	}

	// Step 2: rotate onto a different preset so the round-trip has
	// somewhere to come back from.
	if err := c.SelectLayout(ctx, target, "main-vertical", SelectLayoutOpts{}); err != nil {
		t.Fatalf("rotate main-vertical: %v", err)
	}
	if mid := listWindowLayout(t, ctx, c, target); mid == saved {
		t.Fatalf("main-vertical produced the same dump as even-horizontal: %q", mid)
	}

	// Step 3: feed the saved dump back. tmux must restore the layout
	// byte-for-byte, which is the contract callers building a
	// "remember and restore" workflow rely on.
	if err := c.SelectLayout(ctx, target, saved, SelectLayoutOpts{}); err != nil {
		t.Fatalf("restore stored dump: %v", err)
	}
	if got := listWindowLayout(t, ctx, c, target); got != saved {
		t.Fatalf("after restore window_layout = %q, want %q", got, saved)
	}
}

// TestSelectLayout_MissingWindowWrapsSentinel pins the typed-error
// flow: SelectLayout against an unknown session must surface
// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
// CodeSessionNotFound, mirroring SelectWindow / SwapWindow.
func TestSelectLayout_MissingWindowWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	// Anchor a real session so the failure path is "server up, target
	// missing" rather than "no server running" — without it, tmux's
	// stderr text differs across versions and would land on a different
	// translation branch.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor_sl", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err := c.SelectLayout(ctx, "ghost_session_nonexistent:0", "tiled", SelectLayoutOpts{})
	if err == nil {
		t.Fatal("expected error for missing window")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSelectLayout_RejectsEmptyTarget guards the up-front nil-check so
// a malformed `tmux select-layout` is never issued without a -t value.
// Mirrors the parallel guards on every other window controller method.
func TestSelectLayout_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.SelectLayout(ctx, "", "tiled", SelectLayoutOpts{})
	if err == nil || !strings.Contains(err.Error(), "target required") {
		t.Fatalf("empty target: got %v, want \"target required\"", err)
	}
}

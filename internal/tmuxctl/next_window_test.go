package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// activeIndex returns the index of the window currently flagged active
// for session, or -1 if no window is active in the listing. Pulled out
// so each NextWindow assertion can compare integer indices rather than
// re-walk the slice every time and so the failure message names the
// concrete index involved.
func activeIndex(t *testing.T, c *Controller, ctx context.Context, session string) int {
	t.Helper()
	wins, err := c.ListWindows(ctx, session)
	if err != nil {
		t.Fatalf("ListWindows %q: %v", session, err)
	}
	for _, w := range wins {
		if w.Active {
			return w.Index
		}
	}
	return -1
}

// TestNextWindow_AdvancesByOne pins the load-bearing happy path: with
// three windows in a session, calling NextWindow once must move the
// active flag from index 0 to index 1. Without this an agent that
// chains capture → next_window → capture cannot trust the pointer
// actually moved.
func TestNextWindow_AdvancesByOne(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "nw_adv", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Three windows total (the session-create one plus two more) so the
	// "advance by one" step has somewhere meaningful to land — index 1
	// — that is neither the starting point nor the wrap-around case.
	for _, name := range []string{"second", "third"} {
		if _, err := c.CreateWindow(ctx, WindowSpec{
			Session: "nw_adv", Name: name, Command: "/bin/sh", Select: false,
		}); err != nil {
			t.Fatalf("CreateWindow %q: %v", name, err)
		}
	}

	// Sanity baseline: index 0 is the active window. Without this the
	// post-call assertion could not distinguish a working NextWindow
	// from a no-op.
	if got := activeIndex(t, c, ctx, "nw_adv"); got != 0 {
		t.Fatalf("baseline broken: active index = %d, want 0", got)
	}

	if err := c.NextWindow(ctx, "nw_adv", false); err != nil {
		t.Fatalf("NextWindow: %v", err)
	}
	if got := activeIndex(t, c, ctx, "nw_adv"); got != 1 {
		t.Fatalf("after NextWindow: active index = %d, want 1", got)
	}
}

// TestNextWindow_WrapsAroundAtEnd pins tmux's documented "next-window
// wraps to the first" behaviour. With three windows and the active
// pointer parked on the last one, NextWindow must land on index 0.
// Without this an agent driving a long round-trip (step forward N
// times) cannot reason about where the pointer ends up after a full
// loop.
func TestNextWindow_WrapsAroundAtEnd(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "nw_wrap", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for _, name := range []string{"second", "third"} {
		if _, err := c.CreateWindow(ctx, WindowSpec{
			Session: "nw_wrap", Name: name, Command: "/bin/sh", Select: false,
		}); err != nil {
			t.Fatalf("CreateWindow %q: %v", name, err)
		}
	}

	// Park the active pointer on the last window so the next step has
	// to wrap. Doing it via SelectWindow (rather than two NextWindow
	// calls) keeps the wrap test independent of the "advance by one"
	// behaviour: a regression in either path produces a focused
	// failure.
	if err := c.SelectWindow(ctx, "nw_wrap", "third"); err != nil {
		t.Fatalf("SelectWindow third: %v", err)
	}
	if got := activeIndex(t, c, ctx, "nw_wrap"); got != 2 {
		t.Fatalf("baseline broken: active index = %d, want 2", got)
	}

	if err := c.NextWindow(ctx, "nw_wrap", false); err != nil {
		t.Fatalf("NextWindow: %v", err)
	}
	if got := activeIndex(t, c, ctx, "nw_wrap"); got != 0 {
		t.Fatalf("after wrap NextWindow: active index = %d, want 0", got)
	}
}

// TestNextWindow_MissingSessionWrapsSentinel pins the typed-error
// flow: NextWindow against an unknown session must surface
// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
// CodeSessionNotFound, mirroring SelectWindow / CreateWindow / KillWindow.
// We anchor a real session first so the failure path is "server up,
// session missing" rather than the noisier "no server running".
func TestNextWindow_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := c.CreateSession(ctx, SessionSpec{Name: "nw_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err := c.NextWindow(ctx, "ghost_session_nonexistent", false)
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestNextWindow_RejectsEmptyTarget guards the up-front nil-check so a
// malformed `tmux next-window -t` (with an empty target) is never
// issued. Mirrors the parallel guards on SelectWindow / KillWindow.
func TestNextWindow_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := c.NextWindow(ctx, "", false); err == nil ||
		!strings.Contains(err.Error(), "target required") {
		t.Fatalf("empty target: got %v, want \"target required\"", err)
	}
}

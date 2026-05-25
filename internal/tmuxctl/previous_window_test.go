package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// activeIndex returns the numeric index of the currently active window
// in the named session. Tests use it to assert which slot
// PreviousWindow landed on without re-encoding the parsing in every
// case. A missing-active result is reported via -1 so callers can fail
// the test with a meaningful diagnostic rather than an out-of-range
// panic.
func activeIndex(t *testing.T, c *Controller, ctx context.Context, session string) int {
	t.Helper()
	wins, err := c.ListWindows(ctx, session)
	if err != nil {
		t.Fatalf("ListWindows(%q): %v", session, err)
	}
	for _, w := range wins {
		if w.Active {
			return w.Index
		}
	}
	t.Fatalf("no active window in session %q: %+v", session, wins)
	return -1
}

// TestPreviousWindow_StepsBackward pins the happy path: with three
// windows and the active flag sitting on index 2, PreviousWindow must
// land on index 1. Without this baseline a future regression that
// silently no-ops the call (or accidentally wraps to the last window
// every time) could not be told apart from a working step.
func TestPreviousWindow_StepsBackward(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "pw_step", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Two more windows so the session has indices {0, 1, 2}. Select=true
	// on the second create lands the active flag on index 2 — that's our
	// starting position for the step-backward assertion.
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "pw_step", Name: "mid", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow mid: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "pw_step", Name: "last", Command: "/bin/sh", Select: true,
	}); err != nil {
		t.Fatalf("CreateWindow last: %v", err)
	}

	if got := activeIndex(t, c, ctx, "pw_step"); got != 2 {
		t.Fatalf("baseline broken: active index = %d, want 2", got)
	}

	if err := c.PreviousWindow(ctx, "pw_step", false); err != nil {
		t.Fatalf("PreviousWindow: %v", err)
	}
	if got := activeIndex(t, c, ctx, "pw_step"); got != 1 {
		t.Fatalf("active index after PreviousWindow = %d, want 1", got)
	}
}

// TestPreviousWindow_WrapsFromZeroToLast pins tmux's wrap-around
// behaviour: from the first window of a session, previous-window must
// land on the highest-numbered one rather than refusing the call.
// tmux's own CLI does the same thing, so an agent that walks backward
// through a session can rely on a single contract instead of having to
// guard against the edge.
func TestPreviousWindow_WrapsFromZeroToLast(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "pw_wrap", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "pw_wrap", Name: "mid", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow mid: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "pw_wrap", Name: "last", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow last: %v", err)
	}
	// CreateSession leaves index 0 active, and the two follow-ups used
	// Select=false, so the baseline already sits on the wrap-around edge.
	if got := activeIndex(t, c, ctx, "pw_wrap"); got != 0 {
		t.Fatalf("baseline broken: active index = %d, want 0", got)
	}

	if err := c.PreviousWindow(ctx, "pw_wrap", false); err != nil {
		t.Fatalf("PreviousWindow: %v", err)
	}
	// After wrapping, the active flag must sit on the highest-numbered
	// window — index 2 because we created two extras on top of the
	// auto-created index 0.
	if got := activeIndex(t, c, ctx, "pw_wrap"); got != 2 {
		t.Fatalf("active index after wrap = %d, want 2", got)
	}
}

// TestPreviousWindow_MissingSessionWrapsSentinel pins the typed-error
// flow: PreviousWindow against an unknown session must surface
// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
// CodeSessionNotFound — same contract as SelectWindow / SwapWindow.
func TestPreviousWindow_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise the "server up, session
	// missing" path. Without it, tmux emits "no server running" instead
	// of "can't find session", which would land on a different code path.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor_pw", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.PreviousWindow(ctx, "ghost_session_nonexistent", false)
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestPreviousWindow_RejectsEmptyTarget guards the up-front nil-check
// so a `tmux previous-window -t ""` is never issued; tmux would
// otherwise interpret it against the current/global state, which is
// almost never what an agent meant.
func TestPreviousWindow_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.PreviousWindow(ctx, "", false)
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if !strings.Contains(err.Error(), "target required") {
		t.Fatalf("error = %q, want to contain \"target required\"", err.Error())
	}
}

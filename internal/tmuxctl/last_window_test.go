package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestLastWindow_TogglesBetweenPreviousAndCurrent pins the load-bearing
// behaviour: tmux remembers the last active window per session, so a
// SelectWindow into "1" followed by SelectWindow into "2" leaves "1"
// recorded as the previous slot. LastWindow then flips back to "1"
// without the caller having to remember the index. We use three
// windows (0/1/2) so the assertion is unambiguous — a buggy
// implementation that just decremented the index would land on "1"
// from "2" by coincidence, so we verify by name (`first`) instead.
func TestLastWindow_TogglesBetweenPreviousAndCurrent(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "lw", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Three named windows so we can assert on names rather than
	// indices — index 0 is whatever tmux auto-assigned for the
	// session command, then "first" / "second" follow.
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "lw", Name: "first", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow first: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "lw", Name: "second", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow second: %v", err)
	}

	// Walk the session through "first" → "second" so tmux records
	// "first" as the previous slot.
	if err := c.SelectWindow(ctx, "lw", "first"); err != nil {
		t.Fatalf("SelectWindow first: %v", err)
	}
	if err := c.SelectWindow(ctx, "lw", "second"); err != nil {
		t.Fatalf("SelectWindow second: %v", err)
	}

	// Sanity: confirm the active flag is on "second" before we toggle.
	// Without this baseline a no-op LastWindow would still pass the
	// post-call check below.
	wins, err := c.ListWindows(ctx, "lw")
	if err != nil {
		t.Fatalf("ListWindows pre: %v", err)
	}
	preActive := ""
	for _, w := range wins {
		if w.Active {
			preActive = w.Name
		}
	}
	if preActive != "second" {
		t.Fatalf("baseline: expected active window 'second', got %q (full=%+v)", preActive, wins)
	}

	if lerr := c.LastWindow(ctx, "lw"); lerr != nil {
		t.Fatalf("LastWindow: %v", lerr)
	}

	wins, err = c.ListWindows(ctx, "lw")
	if err != nil {
		t.Fatalf("ListWindows post: %v", err)
	}
	postActive := ""
	for _, w := range wins {
		if w.Active {
			postActive = w.Name
		}
	}
	if postActive != "first" {
		t.Fatalf("LastWindow did not toggle back: active = %q, want %q (full=%+v)",
			postActive, "first", wins)
	}
}

// TestLastWindow_RoundTrip pins the toggling contract: calling
// LastWindow twice in a row must return the session to its starting
// active window. tmux's `last-window` is symmetric — it swaps the
// "current" and "previous" slots — so an even number of calls should
// be a no-op from the caller's perspective.
func TestLastWindow_RoundTrip(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "lwrt", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "lwrt", Name: "alt", Command: "/bin/sh", Select: true,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	// Active window is "alt" right now (Select: true). LastWindow ➔
	// flips to the original window; LastWindow again ➔ flips back.
	if err := c.LastWindow(ctx, "lwrt"); err != nil {
		t.Fatalf("LastWindow first call: %v", err)
	}
	if err := c.LastWindow(ctx, "lwrt"); err != nil {
		t.Fatalf("LastWindow second call: %v", err)
	}

	wins, err := c.ListWindows(ctx, "lwrt")
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	gotActive := ""
	for _, w := range wins {
		if w.Active {
			gotActive = w.Name
		}
	}
	if gotActive != "alt" {
		t.Fatalf("after round-trip: active = %q, want %q (full=%+v)",
			gotActive, "alt", wins)
	}
}

// TestLastWindow_MissingSessionWrapsSentinel pins the typed-error
// contract: LastWindow against an unknown session must surface
// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
// CodeSessionNotFound, mirroring SelectWindow / RenameWindow / SwapWindow.
func TestLastWindow_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so the dispatcher hits the "server up,
	// session missing" branch — without it, tmux can emit "no server
	// running" which lands on a different code path.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err := c.LastWindow(ctx, "ghost_session_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestLastWindow_RejectsEmptyTarget guards the up-front empty-string
// check so a partially-formed `tmux last-window -t ""` is never
// issued (tmux would silently fall back to the current attached
// client, which is rarely what the caller meant).
func TestLastWindow_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.LastWindow(ctx, "")
	if err == nil || !strings.Contains(err.Error(), "target required") {
		t.Fatalf("empty target: got %v, want \"target required\"", err)
	}
}

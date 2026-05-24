package tmuxctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSwapWindow_TradesIndices drives the happy path: with two named
// windows in a session, calling SwapWindow against "0" and "1" must
// leave the layout reflecting the swap — the window that used to live
// at index 0 now lives at 1 (and vice versa). tmux moves the layout
// slots in place, so the assertion is "the names attached to indices
// flipped".
func TestSwapWindow_TradesIndices(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "sw", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "sw", Name: "second", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	// Sanity-check the baseline so the post-swap assertion has a
	// meaningful before-picture: index 0 is the auto-named first window
	// (the session's command), index 1 is "second".
	wins, err := c.ListWindows(ctx, "sw")
	if err != nil {
		t.Fatalf("ListWindows pre: %v", err)
	}
	pre := map[int]string{}
	for _, w := range wins {
		pre[w.Index] = w.Name
	}
	if pre[1] != "second" {
		t.Fatalf("baseline: expected window 1 named %q, got %v", "second", pre)
	}

	if serr := c.SwapWindow(ctx, "sw", "0", "1", true); serr != nil {
		t.Fatalf("SwapWindow: %v", serr)
	}

	wins, err = c.ListWindows(ctx, "sw")
	if err != nil {
		t.Fatalf("ListWindows post: %v", err)
	}
	post := map[int]string{}
	for _, w := range wins {
		post[w.Index] = w.Name
	}
	// After the swap the names must have flipped: whatever was at
	// index 0 is now at 1, and "second" is at 0.
	if post[0] != "second" {
		t.Errorf("post-swap: index 0 = %q, want %q (full=%v)", post[0], "second", post)
	}
	if post[1] != pre[0] {
		t.Errorf("post-swap: index 1 = %q, want %q (full=%v)", post[1], pre[0], post)
	}
}

// TestSwapWindow_NoSelectPreservesActive pins the -d flag plumbing: an
// agent that swaps without flipping focus must observe the session's
// active window stay where it was. Without noSelect=true, tmux can
// shift focus to follow the swap, which would surprise a chained
// send_keys/capture.
func TestSwapWindow_NoSelectPreservesActive(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "swns", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "swns", Name: "second", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	// The session's active window starts at index 0 (the original).
	// Capture which window is active before the swap so the post-swap
	// check can compare names rather than indices (the indices flip).
	wins, err := c.ListWindows(ctx, "swns")
	if err != nil {
		t.Fatalf("ListWindows pre: %v", err)
	}
	var preActive string
	for _, w := range wins {
		if w.Active {
			preActive = w.Name
		}
	}
	if preActive == "" {
		t.Fatalf("baseline: no active window in %+v", wins)
	}

	if serr := c.SwapWindow(ctx, "swns", "0", "1", true); serr != nil {
		t.Fatalf("SwapWindow: %v", serr)
	}

	wins, err = c.ListWindows(ctx, "swns")
	if err != nil {
		t.Fatalf("ListWindows post: %v", err)
	}
	var postActive string
	for _, w := range wins {
		if w.Active {
			postActive = w.Name
		}
	}
	// noSelect=true means the active *window* (by name/identity) stays
	// the same — even though its index moved as part of the swap.
	if postActive != preActive {
		t.Errorf("active window after no-select swap = %q, want %q", postActive, preActive)
	}
}

// TestSwapWindow_MissingSessionWrapsSentinel pins the typed-error
// flow: SwapWindow against an unknown session must surface
// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
// CodeSessionNotFound — same contract as SelectWindow / MoveWindow.
func TestSwapWindow_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	// Anchor with a real session so we exercise the "server up, session
	// missing" path. Without it, tmux emits "no server running" instead
	// of "can't find window", which would land on a different code path.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.SwapWindow(ctx, "ghost_session_nonexistent", "0", "1", false)
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSwapWindow_RejectsEmptyArgs guards every up-front nil-check so a
// `tmux swap-window` is never issued with a partial target string.
func TestSwapWindow_RejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	cases := []struct {
		name              string
		session, src, dst string
		want              string
	}{
		{"empty session", "", "0", "1", "session required"},
		{"empty src", "sw", "", "1", "src required"},
		{"empty dst", "sw", "0", "", "dst required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.SwapWindow(ctx, tc.session, tc.src, tc.dst, false)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if got := err.Error(); got != tc.want {
				t.Fatalf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

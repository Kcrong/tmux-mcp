package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestLinkWindow_SharesWindowAcrossSessions drives the happy path: a
// window living in the source session is linked into a destination
// session, and a follow-up list_windows on each side reflects the share
// — the linked entry appears under both sessions while the source
// session still owns its original window. tmux's link-window keeps the
// `#{window_id}` stable across the two appearances, which is the
// load-bearing invariant of the feature: the same long-running build
// can be observed from a "monitor" session without losing the
// foreground in the working session.
func TestLinkWindow_SharesWindowAcrossSessions(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Two anchor sessions: src holds the original window, dst is the
	// monitor we link into. Both run /bin/sh so the auto-named first
	// window is harmless.
	if err := c.CreateSession(ctx, SessionSpec{Name: "lwsrc", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession src: %v", err)
	}
	if err := c.CreateSession(ctx, SessionSpec{Name: "lwdst", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession dst: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "lwsrc", Name: "shared", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	// Link lwsrc:shared into lwdst at index 1 (next to lwdst's first
	// auto-named window). kill=false because dst index 1 is empty.
	if err := c.LinkWindow(ctx, "lwsrc:shared", "lwdst:1", false); err != nil {
		t.Fatalf("LinkWindow: %v", err)
	}

	// Source side: the original "shared" window must still be present.
	srcWins, err := c.ListWindows(ctx, "lwsrc")
	if err != nil {
		t.Fatalf("ListWindows src: %v", err)
	}
	var foundSrc bool
	for _, w := range srcWins {
		if w.Name == "shared" {
			foundSrc = true
		}
	}
	if !foundSrc {
		t.Errorf("src lost the linked window: %+v", srcWins)
	}

	// Destination side: the linked window must now appear under lwdst
	// too. Names round-trip through link-window verbatim, so the entry
	// is named "shared" on both sides.
	dstWins, err := c.ListWindows(ctx, "lwdst")
	if err != nil {
		t.Fatalf("ListWindows dst: %v", err)
	}
	var foundDst bool
	for _, w := range dstWins {
		if w.Name == "shared" {
			foundDst = true
		}
	}
	if !foundDst {
		t.Errorf("dst missing the linked window: %+v", dstWins)
	}
}

// TestLinkWindow_KillOverwritesExistingDst pins the -k flag plumbing:
// when an entry already occupies the dst slot, kill=true must replace
// it with the linked window instead of erroring. Without -k tmux
// refuses with "index in use", which the second sub-case asserts so a
// future contributor cannot drop the kill plumbing without breaking
// the test.
func TestLinkWindow_KillOverwritesExistingDst(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "lwksrc", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession src: %v", err)
	}
	if err := c.CreateSession(ctx, SessionSpec{Name: "lwkdst", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession dst: %v", err)
	}
	// Source window we want to share into the dst session.
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "lwksrc", Name: "live", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow src: %v", err)
	}
	// Pre-populate the dst index 1 slot so the link must overwrite it
	// (or refuse, depending on the kill flag).
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "lwkdst", Name: "stale", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow dst: %v", err)
	}

	// kill=false: tmux must refuse with an "in use" style error so the
	// caller knows the slot was already occupied.
	noKillErr := c.LinkWindow(ctx, "lwksrc:live", "lwkdst:1", false)
	if noKillErr == nil {
		t.Fatal("expected error when dst slot is occupied and kill=false")
	}
	if !strings.Contains(strings.ToLower(noKillErr.Error()), "in use") {
		t.Logf("note: tmux phrased the no-kill collision as %q", noKillErr.Error())
	}

	// kill=true: the existing "stale" window is destroyed and the
	// linked "live" window takes its place.
	if killErr := c.LinkWindow(ctx, "lwksrc:live", "lwkdst:1", true); killErr != nil {
		t.Fatalf("LinkWindow with kill: %v", killErr)
	}
	dstWins, err := c.ListWindows(ctx, "lwkdst")
	if err != nil {
		t.Fatalf("ListWindows dst: %v", err)
	}
	var liveAtIdx1, staleStillThere bool
	for _, w := range dstWins {
		if w.Index == 1 && w.Name == "live" {
			liveAtIdx1 = true
		}
		if w.Name == "stale" {
			staleStillThere = true
		}
	}
	if !liveAtIdx1 {
		t.Errorf("dst index 1 = (not live), want %q (full=%+v)", "live", dstWins)
	}
	if staleStillThere {
		t.Errorf("stale window survived a kill=true link: %+v", dstWins)
	}
}

// TestLinkWindow_MissingSessionWrapsSentinel pins the typed-error flow:
// LinkWindow against an unknown session must surface
// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
// CodeSessionNotFound — same contract as SwapWindow / SelectWindow.
func TestLinkWindow_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so we exercise the "server up, session
	// missing" path. Without it tmux emits "no server running" instead
	// of "can't find window", which would land on a different code
	// path.
	if err := c.CreateSession(ctx, SessionSpec{Name: "lwanchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.LinkWindow(ctx, "ghost_lw_session:0", "lwanchor:1", false)
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestLinkWindow_RejectsEmptyArgs guards every up-front nil-check so a
// `tmux link-window` is never issued with a partial target string.
func TestLinkWindow_RejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	cases := []struct {
		name     string
		src, dst string
		want     string
	}{
		{"empty src", "", "lwanchor:1", "src required"},
		{"empty dst", "lwsrc:0", "", "dst required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.LinkWindow(ctx, tc.src, tc.dst, false)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if got := err.Error(); got != tc.want {
				t.Fatalf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

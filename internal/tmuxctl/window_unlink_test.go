package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// linkRaw drives `tmux link-window -s <src> -t <dst>` directly through
// the controller's run() so the unlink tests can construct multi-session
// windows without depending on the controller-level LinkWindow helper
// (which lives behind a separate PR). Centralising the raw call keeps
// the test bodies focused on the unlink behaviour they are actually
// asserting.
func linkRaw(t *testing.T, c *Controller, ctx context.Context, src, dst string) {
	t.Helper()
	if _, err := c.run(ctx, "link-window", "-s", src, "-t", dst); err != nil {
		t.Fatalf("link-window %s -> %s: %v", src, dst, err)
	}
}

// TestUnlinkWindow_RemovesLinkLeavesSourceAlive drives the happy path:
// after grafting a window from the source session into a second
// session via `link-window`, calling UnlinkWindow on the destination
// reference removes that slot from the dst session while the original
// window stays present in the source. This is the load-bearing
// invariant of unlink-window — undoing a link must not destroy the
// underlying window when other sessions still reference it.
func TestUnlinkWindow_RemovesLinkLeavesSourceAlive(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "uwsrc", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession src: %v", err)
	}
	if err := c.CreateSession(ctx, SessionSpec{Name: "uwdst", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession dst: %v", err)
	}
	// The window we plan to share into the dst session. Auto-named
	// first windows differ in name across tmux versions, so use a
	// distinct label here we can assert on after the unlink.
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "uwsrc", Name: "shared", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}
	// Graft uwsrc:shared into uwdst at index 1 so dst now sees the same
	// window. Going through raw tmux keeps the test independent of the
	// controller's LinkWindow helper, which lives on a separate PR.
	linkRaw(t, c, ctx, "uwsrc:shared", "uwdst:1")

	if err := c.UnlinkWindow(ctx, "uwdst:1", false); err != nil {
		t.Fatalf("UnlinkWindow: %v", err)
	}

	// Source side: the original "shared" window must still be present.
	// tmux's unlink-window only removes the dst slot, so the underlying
	// window keeps running where it was created.
	srcWins, err := c.ListWindows(ctx, "uwsrc")
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
		t.Errorf("src lost the linked window after unlink: %+v", srcWins)
	}

	// Destination side: the linked entry must be gone. We check by name
	// rather than index because tmux's auto-named first window may sit
	// at index 0 with a version-dependent label.
	dstWins, err := c.ListWindows(ctx, "uwdst")
	if err != nil {
		t.Fatalf("ListWindows dst: %v", err)
	}
	for _, w := range dstWins {
		if w.Name == "shared" {
			t.Errorf("dst still references the unlinked window: %+v", dstWins)
		}
	}
}

// TestUnlinkWindow_KillRemovesLastReference pins the -k branch: when the
// targeted slot is the *only* reference left to a window, kill=true
// makes tmux remove the slot AND destroy the underlying window
// (because no session would have a path to it otherwise). Without -k
// tmux refuses with "session has only one window" / "would be
// destroyed" — that refusal path is exercised in the second sub-case
// so a future contributor cannot drop the kill plumbing without
// breaking the test.
func TestUnlinkWindow_KillRemovesLastReference(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "uwksrc", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession src: %v", err)
	}
	// Two windows in src so unlink without -k against the *src* copy
	// of the linked window does not run into "last window in session"
	// territory before we even get to the kill flag's job.
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "uwksrc", Name: "keepalive", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow keepalive: %v", err)
	}
	// The window we'll eventually unlink. After this point uwksrc holds
	// three windows: the auto-named first, "keepalive", and "ephemeral".
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "uwksrc", Name: "ephemeral", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow ephemeral: %v", err)
	}

	// kill=false against uwksrc:ephemeral — tmux refuses because the
	// window has only one reference (this session), and unlinking it
	// without -k would orphan the underlying window. The exact phrasing
	// varies across tmux builds, so we assert on the error existing and
	// log the message for forensic value rather than locking in a
	// specific string.
	noKillErr := c.UnlinkWindow(ctx, "uwksrc:ephemeral", false)
	if noKillErr == nil {
		t.Fatal("expected error when unlinking the only reference without kill=true")
	}
	t.Logf("note: tmux phrased the no-kill last-reference refusal as %q", noKillErr.Error())

	// kill=true: the unlink proceeds and the underlying window is
	// destroyed. Post-state must show no window named "ephemeral" under
	// uwksrc — but the session itself stays alive because two other
	// windows still hold it together.
	if killErr := c.UnlinkWindow(ctx, "uwksrc:ephemeral", true); killErr != nil {
		t.Fatalf("UnlinkWindow kill=true: %v", killErr)
	}
	srcWins, err := c.ListWindows(ctx, "uwksrc")
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	for _, w := range srcWins {
		if w.Name == "ephemeral" {
			t.Errorf("ephemeral window survived kill=true unlink: %+v", srcWins)
		}
	}
	// And the session is still alive — kill is bound to the targeted
	// window only, not the surrounding session.
	has, err := c.HasSession(ctx, "uwksrc")
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Errorf("session vanished after kill=true unlink of one window")
	}
}

// TestUnlinkWindow_MissingSessionWrapsSentinel pins the typed-error
// flow: UnlinkWindow against an unknown session must surface
// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
// CodeSessionNotFound — same contract as LinkWindow / SwapWindow /
// SelectWindow.
func TestUnlinkWindow_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	// Anchor a real session so the controller's tmux server is up;
	// otherwise tmux would emit "no server running" instead of "can't
	// find window", which is a different code path.
	if err := c.CreateSession(ctx, SessionSpec{Name: "uwanchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.UnlinkWindow(ctx, "ghost_uw_session:0", false)
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
	// Sanity: the wrapped message still carries tmux's original phrasing
	// so debug logs surface the underlying failure.
	if !strings.Contains(strings.ToLower(err.Error()), "can't find") {
		t.Logf("note: tmux phrased the missing-target error as %q", err.Error())
	}
}

// TestUnlinkWindow_RejectsEmptyTarget guards the up-front nil-check so
// `tmux unlink-window` is never issued without a target string. tmux
// would otherwise resolve "" to whatever it considers current, which
// is rarely what an agent meant to ask for.
func TestUnlinkWindow_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.UnlinkWindow(ctx, "", false)
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if got := err.Error(); got != "target required" {
		t.Fatalf("error = %q, want %q", got, "target required")
	}
}

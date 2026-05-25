package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestFindWindow_MatchesByName drives the default-scope happy path: a
// session with a named window matches by substring against the window
// name and surfaces the (session, index, name) triple an agent needs to
// build a follow-up `<session>:<window>` target. Catches the dispatcher
// argv ordering and the parser in one shot.
func TestFindWindow_MatchesByName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "fw_name", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "fw_name", Name: "needle_win", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	got, err := c.FindWindow(ctx, "needle", FindWindowOpts{NameOnly: true})
	if err != nil {
		t.Fatalf("FindWindow: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("matches len = %d, want 1; got=%+v", len(got), got)
	}
	if got[0].Session != "fw_name" {
		t.Errorf("Session = %q, want fw_name", got[0].Session)
	}
	if got[0].WindowName != "needle_win" {
		t.Errorf("WindowName = %q, want needle_win", got[0].WindowName)
	}
	if got[0].WindowIndex < 1 {
		// CreateWindow appends past index 0, so the matching window must
		// be at index >= 1. Without this guard a regression that returned
		// the wrong row (the unrelated index-0 window) could pass.
		t.Errorf("WindowIndex = %d, want >= 1", got[0].WindowIndex)
	}
}

// TestFindWindow_NoMatchReturnsEmptySlice pins the documented "empty
// slice (not nil)" contract for queries that filter to zero rows. A
// caller branching on `len(matches) == 0` should never have to also
// handle a nil shape — it forces an extra branch that is easy to get
// wrong on the JSON encoder side too.
func TestFindWindow_NoMatchReturnsEmptySlice(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "fw_empty", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := c.FindWindow(ctx, "definitely_not_present_xyzzy", FindWindowOpts{
		NameOnly: true,
		Target:   "fw_empty",
	})
	if err != nil {
		t.Fatalf("FindWindow: %v", err)
	}
	if got == nil {
		t.Fatal("FindWindow returned nil slice; want non-nil empty slice for zero matches")
	}
	if len(got) != 0 {
		t.Fatalf("matches len = %d, want 0; got=%+v", len(got), got)
	}
}

// TestFindWindow_RegexMatchesByName covers the `-r` flag: switching
// from fnmatch to a regular expression must let an anchored pattern
// pick out a window whose name starts with the expression — the bare
// substring shape would have hit too, but only the regex shape can
// express the anchor.
func TestFindWindow_RegexMatchesByName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "fw_regex", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "fw_regex", Name: "build_log", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "fw_regex", Name: "post_build", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	// Anchored regex: only `build_log` starts with "build", so `post_build`
	// must not appear in the result. The non-regex variant of the same
	// match ("build") would have matched both names.
	got, err := c.FindWindow(ctx, "^build", FindWindowOpts{
		NameOnly: true, Regex: true, Target: "fw_regex",
	})
	if err != nil {
		t.Fatalf("FindWindow: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("regex matches len = %d, want 1 (only build_log); got=%+v", len(got), got)
	}
	if got[0].WindowName != "build_log" {
		t.Errorf("WindowName = %q, want build_log", got[0].WindowName)
	}
}

// TestFindWindow_TargetRestrictsScope locks the documented `-t
// <session>` semantics: a name that exists in two sessions is filtered
// down to the one named in Opts.Target. Without this pin a regression
// that dropped the -t flag would leak the other session's match.
func TestFindWindow_TargetRestrictsScope(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "fw_a", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession a: %v", err)
	}
	if err := c.CreateSession(ctx, SessionSpec{Name: "fw_b", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession b: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "fw_a", Name: "shared_label", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow a: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "fw_b", Name: "shared_label", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow b: %v", err)
	}

	// Without Target the same query should return both windows; pinning
	// Target=fw_b must collapse the result to only the fw_b row.
	got, err := c.FindWindow(ctx, "shared_label", FindWindowOpts{
		NameOnly: true, Target: "fw_b",
	})
	if err != nil {
		t.Fatalf("FindWindow: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("matches len = %d, want 1 (target should filter out fw_a); got=%+v", len(got), got)
	}
	if got[0].Session != "fw_b" {
		t.Errorf("Session = %q, want fw_b (target was supposed to scope the search)", got[0].Session)
	}
}

// TestFindWindow_RejectsEmptyMatch guards the up-front nil-check: an
// empty match string would otherwise let tmux's filter expression
// degrade into "match every window name", which is never what an agent
// asked for and would mask a typo somewhere up the call stack.
func TestFindWindow_RejectsEmptyMatch(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	_, err := c.FindWindow(ctx, "", FindWindowOpts{})
	if err == nil {
		t.Fatal("expected error for empty match")
	}
	if !strings.Contains(err.Error(), "match required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestFindWindow_MissingTargetWrapsSentinel pins the sentinel contract:
// calling FindWindow with a Target that does not name any session must
// wrap errs.ErrSessionNotFound so the JSON-RPC layer maps to
// CodeSessionNotFound. Without this the boundary would surface
// CodeInternal and break the "branch on code" contract.
func TestFindWindow_MissingTargetWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor so the tmux server is up and the failure path is "server
	// up, session missing" rather than "no server running".
	if err := c.CreateSession(ctx, SessionSpec{Name: "fw_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err := c.FindWindow(ctx, "anything", FindWindowOpts{
		Target: "ghost_session_nonexistent",
	})
	if err == nil {
		t.Fatal("expected error for missing target session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

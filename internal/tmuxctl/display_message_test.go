package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestDisplayMessage_HappyPath drives a real tmux server end-to-end:
// create a session, ask DisplayMessage to evaluate `#{session_name}`
// against it, and assert the resolved line matches the session name.
// The format-string surface is intentionally exercised against a
// well-known variable so the test is independent of tmux's pane
// numbering quirks.
func TestDisplayMessage_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "dm_happy"
	if err := c.CreateSession(ctx, SessionSpec{
		Name: name, Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := c.DisplayMessage(ctx, "#{session_name}", name, "", "")
	if err != nil {
		t.Fatalf("DisplayMessage: %v", err)
	}
	if got != name {
		t.Errorf("DisplayMessage value = %q, want %q", got, name)
	}
}

// TestDisplayMessage_MultiVariable confirms the format DSL passes
// through unchanged: tmux resolves multiple `#{...}` variables in one
// invocation and returns them as a single line, which DisplayMessage
// surfaces verbatim.
func TestDisplayMessage_MultiVariable(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "dm_multi"
	if err := c.CreateSession(ctx, SessionSpec{
		Name: name, Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := c.DisplayMessage(ctx, "#{session_name}|#{window_index}", name, "", "")
	if err != nil {
		t.Fatalf("DisplayMessage: %v", err)
	}
	// tmux's default first window index is 0; assert on the prefix and
	// the literal pipe so the test does not depend on tmux's
	// base-index option, which an operator may have tuned.
	if !strings.HasPrefix(got, name+"|") {
		t.Errorf("DisplayMessage value = %q, want prefix %q", got, name+"|")
	}
}

// TestDisplayMessage_UnknownTargetWrapsSentinel pins the contract the
// JSON-RPC layer relies on: an unknown session surfaces as a wrapped
// errs.ErrSessionNotFound regardless of whether tmux's stderr says
// "can't find session", "can't find window", or "can't find pane".
func TestDisplayMessage_UnknownTargetWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server so we exercise the "server up, target
	// missing" branch (a fresh controller has no socket and produces
	// the different "error connecting" message run() does not
	// translate).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession anchor: %v", err)
	}

	_, err := c.DisplayMessage(ctx, "#{session_name}", "definitely_missing_xyzzy", "", "")
	if err == nil {
		t.Fatal("expected error for unknown target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestDisplayMessage_EmptyFormatRejected guards the cheap input check
// the method performs before shelling out to tmux.
func TestDisplayMessage_EmptyFormatRejected(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if _, err := c.DisplayMessage(ctx, "", "", "", ""); err == nil {
		t.Fatal("expected error for empty format")
	}
}

// TestBuildDisplayTarget covers the small target-assembly helper. The
// rules pin tmux's own target grammar: pane wins, then window, then
// session, then nothing.
func TestBuildDisplayTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                  string
		session, window, pane string
		want                  string
	}{
		{"all empty", "", "", "", ""},
		{"session only", "demo", "", "", "demo"},
		{"session+window", "demo", "0", "", "demo:0"},
		{"session+window+pane", "demo", "0", "1", "demo:0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := buildDisplayTarget(tc.session, tc.window, tc.pane); got != tc.want {
				t.Errorf("buildDisplayTarget(%q,%q,%q) = %q, want %q",
					tc.session, tc.window, tc.pane, got, tc.want)
			}
		})
	}
}

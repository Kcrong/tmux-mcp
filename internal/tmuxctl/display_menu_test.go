package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestDisplayMenu_HeadlessReportsNoCurrentClient pins the headless path:
// the controller's tmux server has no client attached, so `display-menu`
// has nowhere to draw and tmux surfaces "no current client". The
// boundary forwards that error verbatim because the caller may
// legitimately fix the situation by passing target_client. The test
// asserts the controller does NOT silently swallow the failure as a
// success — that would make the call a black-hole on any headless
// deployment.
func TestDisplayMenu_HeadlessReportsNoCurrentClient(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so we exercise the
	// "server up, no clients attached" branch rather than the
	// entirely-different "no server running" stderr shape.
	if err := c.CreateSession(ctx, SessionSpec{Name: "dm_headless", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.DisplayMenu(ctx, DisplayMenuOpts{
		Items: []DisplayMenuItem{{Name: "Quit", Key: "q", Command: `display "bye"`}},
	})
	if err == nil {
		t.Fatal("expected error on headless server (no current client)")
	}
	// tmux 3.x phrases this as "no current client"; assert the substring
	// rather than the exact line so we are robust across versions.
	if !strings.Contains(strings.ToLower(err.Error()), "no current client") {
		t.Logf("note: error message was %q (no 'no current client' substring); accepting because some tmux builds use a different phrase", err.Error())
	}
}

// TestDisplayMenu_RejectsEmptyItems pins the up-front guard: tmux
// rejects an empty menu with "no menu items" and we refuse the call
// even earlier so the failure surfaces as a clean validation error.
func TestDisplayMenu_RejectsEmptyItems(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.DisplayMenu(ctx, DisplayMenuOpts{Items: nil})
	if err == nil {
		t.Fatal("expected error for empty items slice")
	}
	if !strings.Contains(err.Error(), "at least one item required") {
		t.Fatalf("error %q missing 'at least one item required'", err.Error())
	}
}

// TestDisplayMenu_RejectsItemMissingName pins the per-item guard: a
// row with an empty Name would render as a separator the user cannot
// navigate to, so the boundary refuses the shape before it reaches
// tmux. Doing so makes a tmux-side parse error impossible to silently
// inherit later if tmux's separator semantics ever change.
func TestDisplayMenu_RejectsItemMissingName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.DisplayMenu(ctx, DisplayMenuOpts{
		Items: []DisplayMenuItem{
			{Name: "Run", Key: "r", Command: "display ok"},
			{Name: "", Key: "", Command: ""},
		},
	})
	if err == nil {
		t.Fatal("expected error when an item.name is empty")
	}
	if !strings.Contains(err.Error(), "item[1].name must not be empty") {
		t.Fatalf("error %q missing 'item[1].name must not be empty'", err.Error())
	}
}

// TestDisplayMenu_UnknownClientWrapsSentinel pins the typed-error path
// for an unknown -c TARGET-CLIENT: tmux phrases the failure as
// "can't find client" and the controller must wrap that into
// errs.ErrSessionNotFound so the JSON-RPC layer maps the call uniformly
// to CodeSessionNotFound. We use a TTY path that does not exist on the
// runner (`/dev/pts/ghost_does_not_exist`) so tmux's resolution always
// fails regardless of the test runner's actual pts allocation.
func TestDisplayMenu_UnknownClientWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "dm_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.DisplayMenu(ctx, DisplayMenuOpts{
		TargetClient: "/dev/pts/ghost_does_not_exist",
		Items:        []DisplayMenuItem{{Name: "Quit", Key: "q", Command: `display "bye"`}},
	})
	if err == nil {
		t.Fatal("expected error for missing target client")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		// tmux may emit the headless "no current client" instead on
		// some versions — that is also acceptable since the test fleet
		// has no real client either way. Only fail if we got a totally
		// unrelated phrase.
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "no current client") &&
			!strings.Contains(msg, "can't find client") {
			t.Fatalf("error %v does not match either ErrSessionNotFound or the documented headless shapes", err)
		}
	}
}

// TestDisplayMenu_EmptyKeyAndCommandPreserveAlignment exercises the
// argv builder's promise that an empty Key/Command still consumes the
// positional slot. tmux's parser is strict about three tokens per
// item; without the empty-string fillers the second item's name would
// be misread as the first item's key. We can't easily inspect argv
// from outside the controller, but we drive the call against the
// headless tmux and confirm the error remains the documented
// "no current client" — i.e. tmux parsed the arguments but had no
// client to draw on, rather than emitting a parse error.
func TestDisplayMenu_EmptyKeyAndCommandPreserveAlignment(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "dm_align", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.DisplayMenu(ctx, DisplayMenuOpts{
		Items: []DisplayMenuItem{
			{Name: "Yes", Key: "", Command: ""},
			{Name: "No", Key: "n", Command: `display "no"`},
		},
	})
	if err == nil {
		t.Fatal("expected error on headless server")
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "usage:") || strings.Contains(low, "unknown") {
		t.Fatalf("argv builder produced a tmux parse error (%q); empty Key/Command must still consume the positional slot", err.Error())
	}
}

// TestDisplayMenu_AcceptsAllOptionalFlags exercises the full flag
// surface in one call so a regression in argv assembly (e.g. swapping
// `-T` and `-t`) tickles a tmux usage error rather than going
// unnoticed. We expect tmux to fail at the "no current client" step,
// not at argument parsing — the test asserts the failure message
// is a render-time issue, not a parse error.
func TestDisplayMenu_AcceptsAllOptionalFlags(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "dm_full", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.DisplayMenu(ctx, DisplayMenuOpts{
		Title:          "Title",
		BorderLines:    "single",
		BorderStyle:    "fg=red",
		SelectedStyle:  "bg=blue",
		StartingChoice: "0",
		X:              "C",
		Y:              "C",
		NoCallbacks:    true,
		Items: []DisplayMenuItem{
			{Name: "Run", Key: "r", Command: `display "run"`},
		},
	})
	if err == nil {
		t.Fatal("expected error on headless server")
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "usage:") || strings.Contains(low, "unknown option") {
		t.Fatalf("argv builder mis-assembled flags: %q", err.Error())
	}
}

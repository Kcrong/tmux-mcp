package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSelectWindow_MovesActiveFlag pins the happy path: with two
// windows in a session, selecting the second one moves tmux's active
// flag — which is what an agent ultimately observes via list_windows.
func TestSelectWindow_MovesActiveFlag(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "sw", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "sw", Name: "second", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	// Sanity: the second window should not yet be active because we
	// passed Select=false. Without this baseline we could not tell
	// SelectWindow apart from the no-op case.
	wins, err := c.ListWindows(ctx, "sw")
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	for _, w := range wins {
		if w.Name == "second" && w.Active {
			t.Fatalf("baseline broken: 'second' active before SelectWindow: %+v", wins)
		}
	}

	if selErr := c.SelectWindow(ctx, "sw", "second"); selErr != nil {
		t.Fatalf("SelectWindow: %v", selErr)
	}

	wins, err = c.ListWindows(ctx, "sw")
	if err != nil {
		t.Fatalf("ListWindows after select: %v", err)
	}
	sawActive := false
	for _, w := range wins {
		if w.Name == "second" && w.Active {
			sawActive = true
			break
		}
	}
	if !sawActive {
		t.Fatalf("'second' not flagged active after SelectWindow: %+v", wins)
	}
}

// TestSelectWindow_AcceptsNumericIndex covers the alternate target
// form: window indexes are valid tmux targets, and the controller
// must hand them through unchanged.
func TestSelectWindow_AcceptsNumericIndex(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "swi", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "swi", Name: "second", Command: "/bin/sh", Select: true,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	// Index 0 is the original window; selecting it should move the
	// active flag back off "second".
	if err := c.SelectWindow(ctx, "swi", "0"); err != nil {
		t.Fatalf("SelectWindow by index: %v", err)
	}
	wins, err := c.ListWindows(ctx, "swi")
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	for _, w := range wins {
		if w.Index == 0 && !w.Active {
			t.Errorf("index 0 not active after selection: %+v", wins)
		}
	}
}

// TestSelectWindow_MissingSessionWrapsSentinel pins the typed-error
// flow: SelectWindow against an unknown session must surface
// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
// CodeSessionNotFound — same contract as CreateWindow / KillWindow.
func TestSelectWindow_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err := c.SelectWindow(ctx, "ghost_session_nonexistent", "0")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSelectWindow_RejectsEmptyArgs covers both up-front nil-checks so
// a partially-targeted `tmux select-window` is never issued.
func TestSelectWindow_RejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := c.SelectWindow(ctx, "", "0"); err == nil ||
		!strings.Contains(err.Error(), "session required") {
		t.Fatalf("empty session: got %v, want \"session required\"", err)
	}
	if err := c.SelectWindow(ctx, "x", ""); err == nil ||
		!strings.Contains(err.Error(), "target required") {
		t.Fatalf("empty target: got %v, want \"target required\"", err)
	}
}

// TestRenameWindow_UpdatesName drives the happy path: after a rename,
// list_windows surfaces the new label and the old one is gone.
func TestRenameWindow_UpdatesName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "rw", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "rw", Name: "before", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	if err := c.RenameWindow(ctx, "rw", "before", "after"); err != nil {
		t.Fatalf("RenameWindow: %v", err)
	}

	wins, err := c.ListWindows(ctx, "rw")
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	sawAfter, sawBefore := false, false
	for _, w := range wins {
		switch w.Name {
		case "after":
			sawAfter = true
		case "before":
			sawBefore = true
		}
	}
	if !sawAfter {
		t.Fatalf("missing 'after' after rename: %+v", wins)
	}
	if sawBefore {
		t.Fatalf("'before' still present after rename: %+v", wins)
	}
}

// TestRenameWindow_AcceptsNumericIndex confirms the index-style target
// works the same as a named target — tmux resolves both forms
// uniformly and the controller must forward whichever the agent passed.
func TestRenameWindow_AcceptsNumericIndex(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "rwi", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.RenameWindow(ctx, "rwi", "0", "renamed"); err != nil {
		t.Fatalf("RenameWindow by index: %v", err)
	}
	wins, err := c.ListWindows(ctx, "rwi")
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	if len(wins) != 1 || wins[0].Name != "renamed" {
		t.Fatalf("expected single window named 'renamed', got %+v", wins)
	}
}

// TestRenameWindow_MissingSessionWrapsSentinel pins the typed-error
// flow so the JSON-RPC layer can map the failure to
// CodeSessionNotFound, matching every other window method.
func TestRenameWindow_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err := c.RenameWindow(ctx, "ghost_session_nonexistent", "0", "x")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestRenameWindow_RejectsEmptyArgs guards every required parameter
// so a malformed `tmux rename-window` is never issued.
func TestRenameWindow_RejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := c.RenameWindow(ctx, "", "0", "x"); err == nil ||
		!strings.Contains(err.Error(), "session required") {
		t.Fatalf("empty session: got %v, want \"session required\"", err)
	}
	if err := c.RenameWindow(ctx, "s", "", "x"); err == nil ||
		!strings.Contains(err.Error(), "target required") {
		t.Fatalf("empty target: got %v, want \"target required\"", err)
	}
	if err := c.RenameWindow(ctx, "s", "0", ""); err == nil ||
		!strings.Contains(err.Error(), "name required") {
		t.Fatalf("empty name: got %v, want \"name required\"", err)
	}
}

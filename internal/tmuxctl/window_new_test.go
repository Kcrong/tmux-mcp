package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestNewWindow_HappyPath exercises the documented response shape: every
// field of NewWindowResult must be populated by parsing the `-P -F`
// line tmux prints. Catches argv ordering (the -n / -- separator) and
// the multi-field parser.
func TestNewWindow_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "nwhappy", Command: "/bin/sh", Width: 80, Height: 20,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	res, err := c.NewWindow(ctx, "nwhappy", "build", "/bin/sh", -1, true)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if res.Session != "nwhappy" {
		t.Errorf("Session = %q, want nwhappy", res.Session)
	}
	if res.Name != "build" {
		t.Errorf("Name = %q, want build", res.Name)
	}
	if !strings.HasPrefix(res.ID, "@") {
		t.Errorf("ID = %q, want '@N' prefix", res.ID)
	}
	if res.Index <= 0 {
		// First window is index 0; the new one we just created must be
		// strictly greater because tmux always appends past the existing
		// index when no -t target slot is supplied.
		t.Errorf("Index = %d, want > 0", res.Index)
	}
}

// TestNewWindow_BackgroundSelectFalse confirms selectWin=false maps to
// the -d flag — the active window pointer should still be the original
// one after the call returns.
func TestNewWindow_BackgroundSelectFalse(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "nwbg", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := c.NewWindow(ctx, "nwbg", "bg", "/bin/sh", -1, false); err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	out, err := c.run(ctx, "display-message", "-p", "-t", "nwbg", "#{window_name}")
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}
	got := strings.TrimSpace(out)
	if got == "bg" {
		t.Fatalf("active window = %q, expected the original (not bg) when selectWin=false", got)
	}
}

// TestNewWindow_AfterIndexInsertsAfter pins the afterIndex semantics: a
// new window asked to land "after index 0" must end up at index 1, not
// appended at the end of the list. Without the targeted insert, tmux
// would assign the next free index — usually 2 once the test seeded a
// non-target window.
func TestNewWindow_AfterIndexInsertsAfter(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "nwafter", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	res, err := c.NewWindow(ctx, "nwafter", "after-zero", "/bin/sh", 0, false)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if res.Index != 1 {
		t.Fatalf("Index = %d, want 1 when inserted after index 0", res.Index)
	}
}

// TestNewWindow_MissingSessionWrapsSentinel pins the sentinel contract:
// calling NewWindow against an unknown session must surface
// errs.ErrSessionNotFound so the JSON-RPC layer maps to
// CodeSessionNotFound.
func TestNewWindow_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor so we exercise the "server up, session missing" branch.
	if err := c.CreateSession(ctx, SessionSpec{Name: "nwanchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err := c.NewWindow(ctx, "ghost_session_nonexistent", "", "", -1, true)
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestNewWindow_RejectsEmptySession locks the up-front guard so a
// `tmux new-window` is never issued with an empty target string —
// tmux would otherwise resolve "" to whatever it considers current,
// which is rarely what an agent meant to ask for.
func TestNewWindow_RejectsEmptySession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	_, err := c.NewWindow(ctx, "", "", "", -1, true)
	if err == nil {
		t.Fatal("expected error for empty session")
	}
	if !strings.Contains(err.Error(), "session required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

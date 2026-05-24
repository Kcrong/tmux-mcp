package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestCreateWindow_NamedAndCommand exercises the happy path: the new
// window appears in list-windows under the requested name. Catches
// argv ordering (the -n / -- separator) and the -P/-F output parser.
func TestCreateWindow_NamedAndCommand(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "cw", Command: "/bin/sh", Width: 80, Height: 20,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	res, err := c.CreateWindow(ctx, WindowSpec{
		Session: "cw", Name: "build", Command: "/bin/sh", Select: true,
	})
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}
	if res.Session != "cw" {
		t.Errorf("Session = %q, want cw", res.Session)
	}
	if res.Name != "build" {
		t.Errorf("Name = %q, want build", res.Name)
	}
	if res.Index == "" {
		t.Errorf("Index empty, want a numeric tmux index")
	}

	out, err := c.run(ctx, "list-windows", "-t", "cw", "-F", "#{window_name}")
	if err != nil {
		t.Fatalf("list-windows: %v", err)
	}
	if !strings.Contains(out, "build") {
		t.Fatalf("list-windows missing 'build' window: %s", out)
	}
}

// TestCreateWindow_DefaultNameWhenEmpty proves we still get a usable
// WindowResult when the caller omits the name — tmux auto-assigns a
// label and the parser must surface whatever tmux reports.
func TestCreateWindow_DefaultNameWhenEmpty(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "cwd", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	res, err := c.CreateWindow(ctx, WindowSpec{Session: "cwd"})
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}
	if res.Name == "" {
		t.Errorf("Name empty even though tmux always assigns one")
	}
	if res.Index == "" {
		t.Errorf("Index empty, want a numeric tmux index")
	}
}

// TestCreateWindow_BackgroundFlag confirms Select=false maps to the -d
// flag — the active window pointer should still be the original one.
func TestCreateWindow_BackgroundFlag(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "cwb", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "cwb", Name: "bg", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	out, err := c.run(ctx, "display-message", "-p", "-t", "cwb", "#{window_name}")
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}
	got := strings.TrimSpace(out)
	if got == "bg" {
		t.Fatalf("active window = %q, expected the original (not bg) when Select=false", got)
	}
}

// TestCreateWindow_MissingSessionWrapsSentinel pins the sentinel
// contract: calling CreateWindow against an unknown session must
// surface errs.ErrSessionNotFound.
func TestCreateWindow_MissingSessionWrapsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Anchor so we exercise the "server up, session missing" branch.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err := c.CreateWindow(ctx, WindowSpec{Session: "ghost_session_nonexistent"})
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestCreateWindow_RejectsEmptySession locks the up-front guard. tmux
// would otherwise resolve "" to whatever it considers current, which
// is rarely what an agent meant to ask for.
func TestCreateWindow_RejectsEmptySession(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.CreateWindow(ctx, WindowSpec{Session: ""})
	if err == nil {
		t.Fatal("expected error for empty session")
	}
	if !strings.Contains(err.Error(), "session required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestKillWindow_RemovesNonLastWindow proves the happy path: with two
// windows in a session, killing the second one leaves the session
// alive with one window remaining.
func TestKillWindow_RemovesNonLastWindow(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "kw", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "kw", Name: "second", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	if err := c.KillWindow(ctx, "kw", "second"); err != nil {
		t.Fatalf("KillWindow: %v", err)
	}

	out, err := c.run(ctx, "list-windows", "-t", "kw", "-F", "#{window_name}")
	if err != nil {
		t.Fatalf("list-windows: %v", err)
	}
	if strings.Contains(out, "second") {
		t.Fatalf("'second' still present after KillWindow: %s", out)
	}
	// The original window must still exist — i.e. the session is alive.
	has, err := c.HasSession(ctx, "kw")
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Fatal("session vanished after killing a non-last window")
	}
}

// TestCountWindows_ReturnsExpected pins the contract used by the
// boundary "is this the last window?" check. After a session with two
// windows is set up, CountWindows must return 2.
func TestCountWindows_ReturnsExpected(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "cn", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got, err := c.CountWindows(ctx, "cn"); err != nil || got != 1 {
		t.Fatalf("CountWindows after create = (%d, %v), want (1, nil)", got, err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "cn", Name: "extra", Command: "/bin/sh", Select: false,
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}
	if got, err := c.CountWindows(ctx, "cn"); err != nil || got != 2 {
		t.Fatalf("CountWindows after add = (%d, %v), want (2, nil)", got, err)
	}
}

// TestCountWindows_MissingSessionWrapsSentinel locks the typed sentinel
// flow so the boundary can map "session not found" to
// CodeSessionNotFound — the same contract every other tmuxctl method
// upholds.
func TestCountWindows_MissingSessionWrapsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err := c.CountWindows(ctx, "ghost_session_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestKillWindow_MissingSessionWrapsSentinel checks the typed sentinel
// flows out of KillWindow when the session does not exist. Without
// this the JSON-RPC layer would return CodeInternal instead of
// CodeSessionNotFound, breaking the "branch on code" contract.
func TestKillWindow_MissingSessionWrapsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err := c.KillWindow(ctx, "ghost_session_nonexistent", "0")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestKillWindow_RejectsEmptyArgs guards both up-front nil-checks so a
// `tmux kill-window` is never issued with a partial target string.
func TestKillWindow_RejectsEmptyArgs(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.KillWindow(ctx, "", "0"); err == nil ||
		!strings.Contains(err.Error(), "session required") {
		t.Fatalf("empty session: got %v, want \"session required\"", err)
	}
	if err := c.KillWindow(ctx, "x", ""); err == nil ||
		!strings.Contains(err.Error(), "window required") {
		t.Fatalf("empty window: got %v, want \"window required\"", err)
	}
}

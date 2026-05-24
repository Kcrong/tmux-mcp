package tmuxctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestChooseTree_AllScope_WalksEverySession exercises the server-wide
// (empty scope) path: every window across every session must surface
// in one call, with the active flag landing on whichever window tmux
// reports as focused.
func TestChooseTree_AllScope_WalksEverySession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	for _, name := range []string{"alpha", "beta"} {
		if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh"}); err != nil {
			t.Fatalf("CreateSession %s: %v", name, err)
		}
	}

	rows, err := c.ChooseTree(ctx, "")
	if err != nil {
		t.Fatalf("ChooseTree(\"\"): %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("expected at least 2 rows across 2 sessions, got %d (%+v)", len(rows), rows)
	}
	seen := map[string]ChooseTreeRow{}
	for _, r := range rows {
		seen[r.Session] = r
		if r.PaneCount < 1 {
			t.Errorf("PaneCount = %d for %s, want >= 1", r.PaneCount, r.Session)
		}
	}
	for _, name := range []string{"alpha", "beta"} {
		if _, ok := seen[name]; !ok {
			t.Errorf("missing session %s in choose-tree snapshot: %+v", name, rows)
		}
	}
}

// TestChooseTree_SessionScope_ScopesToOneSession pins the
// session-scoped path: only windows of the named session must appear,
// and an unrelated sibling session must be filtered out entirely.
func TestChooseTree_SessionScope_ScopesToOneSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "main", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession main: %v", err)
	}
	if err := c.CreateSession(ctx, SessionSpec{Name: "other", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession other: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "main", Name: "side", Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	rows, err := c.ChooseTree(ctx, "session main")
	if err != nil {
		t.Fatalf("ChooseTree(session main): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows for session main, got %d (%+v)", len(rows), rows)
	}
	for _, r := range rows {
		if r.Session != "main" {
			t.Errorf("Session = %q on a session-scoped call, want main", r.Session)
		}
	}
}

// TestChooseTree_WindowScope_ScopesToOneWindow pins the window-scoped
// path: passing "window <session>:<index>" must return exactly one row
// for that window, with the right index and name.
func TestChooseTree_WindowScope_ScopesToOneWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "ws", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.CreateWindow(ctx, WindowSpec{
		Session: "ws", Name: "build", Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	rows, err := c.ChooseTree(ctx, "window ws:build")
	if err != nil {
		t.Fatalf("ChooseTree(window ws:build): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 row for window scope, got %d (%+v)", len(rows), rows)
	}
	r := rows[0]
	if r.Session != "ws" {
		t.Errorf("Session = %q, want ws", r.Session)
	}
	if r.WindowName != "build" {
		t.Errorf("WindowName = %q, want build", r.WindowName)
	}
}

// TestChooseTree_MissingSessionWrapsSentinel pins the typed-error
// contract for an unknown session: callers (and the JSON-RPC layer)
// must be able to errors.Is into errs.ErrSessionNotFound regardless of
// which exact phrase tmux emitted, so the dispatcher can map it
// uniformly to CodeSessionNotFound.
func TestChooseTree_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so we exercise
	// "server up, session missing" rather than the "no server running"
	// branch (different stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err := c.ChooseTree(ctx, "session ghost_session_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestChooseTree_RejectsUnknownScope keeps the scope parser honest: a
// stray prefix that isn't "session " or "window " (or empty) must be
// rejected before any tmux command runs, so a typo on the boundary
// cannot accidentally fall through to the unscoped form.
func TestChooseTree_RejectsUnknownScope(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)

	if _, err := c.ChooseTree(ctx, "pane demo:0.0"); err == nil {
		t.Fatal("expected error for unknown scope prefix")
	}
}

// TestChooseTree_RejectsBareSessionScope guards the empty-name branch:
// "session " on its own (without a name) must error rather than falling
// through and asking tmux to resolve an empty target.
func TestChooseTree_RejectsBareSessionScope(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)

	if _, err := c.ChooseTree(ctx, "session "); err == nil {
		t.Fatal("expected error for bare 'session ' scope")
	}
}

// TestParseChooseTreeLine_BadFieldCount keeps the format-string parser
// honest — drift between chooseTreeFormat and parseChooseTreeLine
// should not silently produce zero-valued ChooseTreeRows.
func TestParseChooseTreeLine_BadFieldCount(t *testing.T) {
	t.Parallel()
	if _, err := parseChooseTreeLine("only|three|fields"); err == nil {
		t.Fatal("expected error when row has too few fields")
	}
}

// TestParseChooseTreeLine_HappyPath round-trips a synthetic row through
// the parser and confirms every field decodes cleanly. This pins the
// chooseTreeFormat ordering: a swap inside the format string that goes
// unnoticed by the integration tests would still trip this one.
func TestParseChooseTreeLine_HappyPath(t *testing.T) {
	t.Parallel()
	got, err := parseChooseTreeLine("demo|0|shell|1|1")
	if err != nil {
		t.Fatalf("parseChooseTreeLine: %v", err)
	}
	if got.Session != "demo" {
		t.Errorf("Session = %q, want demo", got.Session)
	}
	if got.WindowIndex != 0 {
		t.Errorf("WindowIndex = %d, want 0", got.WindowIndex)
	}
	if got.WindowName != "shell" {
		t.Errorf("WindowName = %q, want shell", got.WindowName)
	}
	if got.PaneCount != 1 {
		t.Errorf("PaneCount = %d, want 1", got.PaneCount)
	}
	if !got.Active {
		t.Error("Active = false, want true for window_active=1 row")
	}
}

// TestParseChooseTreeLine_InactiveFlag pins the active=0 branch so a
// future regression where every row looks active is loud.
func TestParseChooseTreeLine_InactiveFlag(t *testing.T) {
	t.Parallel()
	got, err := parseChooseTreeLine("demo|3|shell|2|0")
	if err != nil {
		t.Fatalf("parseChooseTreeLine: %v", err)
	}
	if got.Active {
		t.Fatal("Active = true, want false for window_active=0 row")
	}
}

// TestParseChooseTreeLine_BadIndex guards the parsing path: a non-
// numeric value in the window_index column must surface as an error
// rather than silently producing a zero-index ChooseTreeRow.
func TestParseChooseTreeLine_BadIndex(t *testing.T) {
	t.Parallel()
	if _, err := parseChooseTreeLine("demo|notanint|shell|1|1"); err == nil {
		t.Fatal("expected error for non-numeric window_index")
	}
}

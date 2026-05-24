package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSplitPane_VerticalCreatesSecondPane runs the happy path: after a
// vertical split the session must report two panes, and the parser
// must surface a usable `%N` id plus the 0-based index. This is the
// shape every chained tool (pane_select, send_keys) relies on.
func TestSplitPane_VerticalCreatesSecondPane(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "spv", Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	res, err := c.SplitPane(ctx, SplitOptions{
		Session:   "spv",
		Direction: "vertical",
		Detach:    true,
	})
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if !strings.HasPrefix(res.ID, "%") {
		t.Errorf("SplitResult.ID = %q, want a tmux %%N identifier", res.ID)
	}
	if res.Index < 0 {
		t.Errorf("SplitResult.Index = %d, want >= 0", res.Index)
	}

	panes, err := c.ListPanes(ctx, "spv")
	if err != nil {
		t.Fatalf("ListPanes after split: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("ListPanes after vertical split returned %d panes, want 2", len(panes))
	}
}

// TestSplitPane_HorizontalCreatesSecondPane mirrors the vertical case
// but exercises the -h flag — same end-state assertion (panes==2)
// because tmux exposes the same #{pane_id} surface either way.
func TestSplitPane_HorizontalCreatesSecondPane(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "sph", Command: "/bin/sh", Width: 100, Height: 30,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := c.SplitPane(ctx, SplitOptions{
		Session:   "sph",
		Direction: "horizontal",
		Detach:    true,
	}); err != nil {
		t.Fatalf("SplitPane horizontal: %v", err)
	}

	panes, err := c.ListPanes(ctx, "sph")
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("ListPanes after horizontal split returned %d panes, want 2", len(panes))
	}
}

// TestSplitPane_RunsCommand confirms that a non-empty Command actually
// reaches tmux: we send a sentinel via /bin/sh -c, then use
// WaitForStable on the new pane to assert the output landed. This
// catches the argv-ordering bug where the command would be appended
// before -t and tmux would interpret it as a target.
func TestSplitPane_RunsCommand(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "spc", Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	res, err := c.SplitPane(ctx, SplitOptions{
		Session:   "spc",
		Direction: "vertical",
		Command:   "/bin/sh -c 'echo HELLO_SPLIT_CMD; sleep 60'",
		Detach:    true,
	})
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if res.ID == "" {
		t.Fatal("SplitPane returned empty ID")
	}

	// Capture the new pane directly via its %N id so the assertion is
	// independent of which pane tmux currently considers active.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, capErr := c.run(ctx, "capture-pane", "-p", "-t", res.ID)
		if capErr != nil {
			t.Fatalf("capture-pane: %v", capErr)
		}
		if strings.Contains(out, "HELLO_SPLIT_CMD") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("HELLO_SPLIT_CMD never appeared in pane %s", res.ID)
}

// TestSplitPane_MissingSessionWrapsSentinel pins the typed sentinel
// flow so the JSON-RPC layer can map "session not found" to
// CodeSessionNotFound — the same contract every other tmuxctl method
// upholds.
func TestSplitPane_MissingSessionWrapsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Anchor with a real session so we exercise "server up, session
	// missing" rather than "no server" (different stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err := c.SplitPane(ctx, SplitOptions{
		Session:   "ghost_session_nonexistent",
		Direction: "vertical",
		Detach:    true,
	})
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSplitPane_RejectsEmptySession locks the up-front guard. tmux
// would otherwise resolve "" to whatever it considers current.
func TestSplitPane_RejectsEmptySession(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.SplitPane(ctx, SplitOptions{Direction: "vertical"})
	if err == nil {
		t.Fatal("expected error for empty session")
	}
	if !strings.Contains(err.Error(), "session required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSplitPane_RejectsBadDirection guards the controller-side check
// against unknown direction strings — the boundary catches this first
// with a CodeInvalidParams reply, but the controller still refuses
// rather than silently passing nothing to tmux.
func TestSplitPane_RejectsBadDirection(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "bd", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err := c.SplitPane(ctx, SplitOptions{
		Session:   "bd",
		Direction: "diagonal",
	})
	if err == nil {
		t.Fatal("expected error for bad direction")
	}
	if !strings.Contains(err.Error(), "direction") {
		t.Fatalf("unexpected error: %v", err)
	}
}

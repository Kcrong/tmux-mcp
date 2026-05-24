package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestResizePane_ChangesPaneHeight drives the happy path: split the
// session into two stacked panes, capture the bottom pane's initial
// height, ask tmux to grow it by 5 rows via ResizePane(direction=up),
// and assert the height actually changed in the documented direction.
// "up" on the bottom pane shifts the boundary upward, which makes the
// bottom pane taller — so the post-resize height should be greater
// than the baseline.
func TestResizePane_ChangesPaneHeight(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "rp", Command: "/bin/sh", Width: 120, Height: 40,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// vertical = stacked panes (-v); the new pane lands underneath the
	// original, so resizing it "up" by N rows must grow it by N.
	if _, err := c.SplitPane(ctx, SplitOptions{
		Session:   "rp",
		Direction: "vertical",
		Detach:    true,
	}); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}

	panes, err := c.ListPanes(ctx, "rp")
	if err != nil {
		t.Fatalf("ListPanes pre-resize: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("ListPanes after split = %d, want 2", len(panes))
	}
	// Pane index 1 is the new bottom pane created by SplitPane.
	var before Pane
	for _, p := range panes {
		if p.Index == 1 {
			before = p
		}
	}
	if before.ID == "" {
		t.Fatalf("did not find pane index 1 in %#v", panes)
	}

	if rerr := c.ResizePane(ctx, before.ID, "up", 5); rerr != nil {
		t.Fatalf("ResizePane: %v", rerr)
	}

	panes, err = c.ListPanes(ctx, "rp")
	if err != nil {
		t.Fatalf("ListPanes post-resize: %v", err)
	}
	var after Pane
	for _, p := range panes {
		if p.ID == before.ID {
			after = p
		}
	}
	if after.ID == "" {
		t.Fatalf("pane %s vanished after resize", before.ID)
	}
	if after.Height <= before.Height {
		t.Fatalf("ResizePane up by 5 did not grow the bottom pane: before=%d after=%d",
			before.Height, after.Height)
	}
}

// TestResizePane_MissingTargetWrapsSentinel pins the typed-error
// contract for an unknown target: callers (and the JSON-RPC layer) must
// be able to errors.Is into errs.ErrSessionNotFound regardless of which
// exact phrase tmux emitted ("can't find pane" vs "session not found").
func TestResizePane_MissingTargetWrapsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Anchor with a real session so we exercise "server up, pane missing"
	// rather than "no server" (different stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.ResizePane(ctx, "ghost_session_nonexistent:0.0", "up", 5)
	if err == nil {
		t.Fatal("expected error for missing pane")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestResizePane_RejectsBadDirection guards the controller-side
// whitelist. The boundary already rejects unknown directions with
// CodeInvalidParams, but the controller still refuses rather than
// passing a bogus flag to tmux.
func TestResizePane_RejectsBadDirection(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.ResizePane(ctx, "anywhere:0.0", "diagonal", 5)
	if err == nil {
		t.Fatal("expected error for bad direction")
	}
	if !strings.Contains(err.Error(), "direction") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestResizePane_RejectsEmptyTarget locks the up-front guard. tmux
// would otherwise resolve "" to whatever pane it considers current.
func TestResizePane_RejectsEmptyTarget(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.ResizePane(ctx, "", "up", 5)
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if !strings.Contains(err.Error(), "target required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestResizePane_RejectsZeroAmount mirrors the boundary guard:
// resize-pane with a zero step is a no-op tmux silently accepts, which
// is almost never what the caller meant.
func TestResizePane_RejectsZeroAmount(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.ResizePane(ctx, "demo:0.0", "up", 0)
	if err == nil {
		t.Fatal("expected error for zero amount")
	}
	if !strings.Contains(err.Error(), "amount") {
		t.Fatalf("unexpected error: %v", err)
	}
}

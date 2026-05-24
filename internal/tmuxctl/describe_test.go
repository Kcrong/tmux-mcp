package tmuxctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestDescribeSession_HappyPath drives a real tmux server end-to-end:
// create a session with known dimensions, ask DescribeSession to read
// it back, and assert every metadata field is sensible. The bounds we
// check are intentionally loose ("looks like a real session") because
// the exact values depend on the tmux version on PATH.
func TestDescribeSession_HappyPath(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const name = "describe_happy"
	if err := c.CreateSession(ctx, SessionSpec{
		Name: name, Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	before := time.Now().Add(-1 * time.Minute)
	info, err := c.DescribeSession(ctx, name)
	if err != nil {
		t.Fatalf("DescribeSession: %v", err)
	}
	after := time.Now().Add(1 * time.Minute)

	if info.Name != name {
		t.Errorf("Name = %q, want %q", info.Name, name)
	}
	if info.Windows < 1 {
		t.Errorf("Windows = %d, want >= 1", info.Windows)
	}
	if info.Panes < 1 {
		t.Errorf("Panes = %d, want >= 1", info.Panes)
	}
	if info.Width < 20 {
		t.Errorf("Width = %d, want >= 20", info.Width)
	}
	if info.Height < 5 {
		t.Errorf("Height = %d, want >= 5", info.Height)
	}
	if info.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if info.CreatedAt.Before(before) || info.CreatedAt.After(after) {
		t.Errorf("CreatedAt = %v, want between %v and %v", info.CreatedAt, before, after)
	}
}

// TestDescribeSession_UnknownReturnsSentinel pins the contract relied on
// by the JSON-RPC layer: an unknown session name surfaces as a wrapped
// errs.ErrSessionNotFound so the dispatcher can map it to
// CodeSessionNotFound.
func TestDescribeSession_UnknownReturnsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Anchor the tmux server with a real session so we exercise the
	// "server up, named session missing" branch (a fresh controller
	// has no socket and produces a different error message).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession anchor: %v", err)
	}

	_, err := c.DescribeSession(ctx, "definitely_does_not_exist_xyzzy")
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestDescribeSession_EmptyNameRejected guards the cheap input check
// the method performs before shelling out to tmux.
func TestDescribeSession_EmptyNameRejected(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.DescribeSession(ctx, ""); err == nil {
		t.Fatal("expected error for empty session name")
	}
}

// TestDescribeSession_TracksMultiplePanes sanity-checks the pane count
// path: split a window so the session has more than one pane and assert
// DescribeSession picks that up.
func TestDescribeSession_TracksMultiplePanes(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const name = "describe_panes"
	if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh", Width: 80, Height: 24}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Drive a split via the tmux CLI directly through our run helper so
	// we don't have to expose split-window in the public API just for a
	// test. Two panes in a single window is enough to exercise the
	// counting path.
	if _, err := c.run(ctx, "split-window", "-t", name, "-d"); err != nil {
		t.Fatalf("split-window: %v", err)
	}

	info, err := c.DescribeSession(ctx, name)
	if err != nil {
		t.Fatalf("DescribeSession: %v", err)
	}
	if info.Panes < 2 {
		t.Errorf("Panes = %d, want >= 2 after split-window", info.Panes)
	}
}

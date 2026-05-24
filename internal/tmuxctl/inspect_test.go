package tmuxctl

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestInspectSession_HappyPath drives a real tmux server end-to-end:
// create a session running /bin/sh, ask InspectSession to read it back,
// and assert every field looks sensible. The bounds we check are
// intentionally loose ("looks like a real pane") because the exact PID
// and command name depend on the runtime.
func TestInspectSession_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const name = "inspect_happy"
	if err := c.CreateSession(ctx, SessionSpec{
		Name: name, Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	info, err := c.InspectSession(ctx, name)
	if err != nil {
		t.Fatalf("InspectSession: %v", err)
	}

	// PID must be a positive integer; tmux exposes the pane's
	// foreground process and PID 0 would mean "no process", which is
	// impossible immediately after new-session.
	if info.PID <= 0 {
		t.Errorf("PID = %d, want > 0", info.PID)
	}
	// Cwd must be a non-empty absolute path. tmux reports the pane's
	// current working directory; even with no -c the new shell inherits
	// the controller's cwd.
	if info.Cwd == "" {
		t.Error("Cwd is empty")
	}
	if !filepath.IsAbs(info.Cwd) {
		t.Errorf("Cwd = %q, want absolute path", info.Cwd)
	}
	// Command must be a non-empty short name (no path, no args).
	if info.Command == "" {
		t.Error("Command is empty")
	}
}

// TestInspectSession_UnknownReturnsSentinel pins the contract relied on
// by the JSON-RPC layer: an unknown session name surfaces as a wrapped
// errs.ErrSessionNotFound so the dispatcher can map it to
// CodeSessionNotFound.
func TestInspectSession_UnknownReturnsSentinel(t *testing.T) {
	t.Parallel()
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

	_, err := c.InspectSession(ctx, "definitely_does_not_exist_xyzzy")
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestInspectSession_EmptyNameRejected guards the cheap input check
// the method performs before shelling out to tmux.
func TestInspectSession_EmptyNameRejected(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.InspectSession(ctx, ""); err == nil {
		t.Fatal("expected error for empty session name")
	}
}

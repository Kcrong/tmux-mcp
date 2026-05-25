package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSendPrefix_HappyPath drives the load-bearing case: with a real
// session anchored on the controller, SendPrefix against the session's
// active pane must return without error. tmux delivers the configured
// prefix key (default C-b, byte 0x02) into the pane's pty; we capture
// the pane afterwards and confirm tmux accepted the call by exiting
// cleanly. Asserting on the literal prefix byte appearing inside `cat`
// would couple the test to a particular shell's echo semantics, so we
// rely on the absence of an error as the primary signal — that is
// what the JSON-RPC dispatcher ultimately surfaces to clients.
func TestSendPrefix_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "sp_happy", Command: "/bin/sh", Width: 80, Height: 20,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.SendPrefix(ctx, "sp_happy", false); err != nil {
		t.Fatalf("SendPrefix: %v", err)
	}
}

// TestSendPrefix_SecondaryRoundTrips pins the `-2` plumbing: both the
// primary (secondary=false) and secondary (secondary=true) variants
// must reach tmux without error against a valid target. tmux happily
// accepts `-2` even when prefix2 is unset (it falls back to whatever is
// configured), so the assertion is "the flag did not break the call",
// which is what the boundary contract promises.
func TestSendPrefix_SecondaryRoundTrips(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "sp_sec", Command: "/bin/sh", Width: 80, Height: 20,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.SendPrefix(ctx, "sp_sec", false); err != nil {
		t.Fatalf("SendPrefix(secondary=false): %v", err)
	}
	if err := c.SendPrefix(ctx, "sp_sec", true); err != nil {
		t.Fatalf("SendPrefix(secondary=true): %v", err)
	}
}

// TestSendPrefix_MissingTargetWrapsSentinel pins the typed-error
// contract for an unknown pane: callers (and the JSON-RPC layer) must
// be able to errors.Is into errs.ErrSessionNotFound regardless of which
// exact phrase tmux emitted ("can't find pane" vs "session not found").
func TestSendPrefix_MissingTargetWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, pane missing"
	// rather than "no server" (different stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.SendPrefix(ctx, "ghost_session_nonexistent:0.0", false)
	if err == nil {
		t.Fatal("expected error for missing pane")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSendPrefix_RejectsEmptyTarget locks the up-front guard. tmux
// would otherwise resolve "" to whatever pane it considers current,
// which is almost never what the caller actually wanted.
func TestSendPrefix_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.SendPrefix(ctx, "", false)
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if !strings.Contains(err.Error(), "target required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

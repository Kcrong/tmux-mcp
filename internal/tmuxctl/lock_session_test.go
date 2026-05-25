package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestLockSession_HappyPath drives the controller end-to-end against an
// existing session: create the session, override its `lock-command` to
// a no-op (`true`) so tmux does not try to fork the default `lock -np`
// against a CI runner without a TTY, then call LockSession. tmux exits
// 0 even on headless servers because the loop over attached clients is
// empty — that is the contract every operator deployment relies on for
// the "secure the screen" primitive.
//
// The lock-command override is the load-bearing precondition: without
// it, a future tmux build that decides to invoke the lock command
// before iterating attached clients would surface a non-zero exit and
// false-flag the test. Pinning the lock-command to `true` keeps the
// behaviour deterministic regardless of which tmux happens to be on
// PATH.
func TestLockSession_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "lock_happy"
	if err := c.CreateSession(ctx, SessionSpec{
		Name: name, Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Override lock-command to a no-op so the test passes without a
	// TTY. tmux's default `lock -np` would otherwise fork in a CI
	// environment that has no /dev/tty — even though the loop over
	// attached clients is empty on a headless server, future tmux
	// builds may still pre-spawn the lock binary, which would surface
	// as a confusing "lock: command not found" stderr instead of a
	// clean exit 0.
	if _, err := c.run(ctx, "set-option", "-t", name, "lock-command", "true"); err != nil {
		t.Fatalf("set-option lock-command: %v", err)
	}

	if err := c.LockSession(ctx, name); err != nil {
		t.Fatalf("LockSession(%q): %v", name, err)
	}
}

// TestLockSession_MissingSessionWrapsSentinel pins the typed-error
// contract for an unknown session: callers (and the JSON-RPC layer)
// must be able to errors.Is into errs.ErrSessionNotFound regardless of
// which exact phrase tmux emitted, so the dispatcher can map every
// "session not found" surface onto CodeSessionNotFound uniformly.
func TestLockSession_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, named
	// session missing" rather than "no server" — the two cases produce
	// different stderr shapes in tmux.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession anchor: %v", err)
	}

	err := c.LockSession(ctx, "definitely_does_not_exist_xyzzy")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestLockSession_RejectsEmptySession locks the up-front guard. tmux
// would otherwise resolve "" to whatever session it considers current,
// which is almost never what the caller actually wanted — the boundary
// validator in the server layer normally prevents this from happening,
// but defending here keeps the controller usable from tests and other
// programmatic callers that bypass the validator.
func TestLockSession_RejectsEmptySession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.LockSession(ctx, "")
	if err == nil {
		t.Fatal("expected error for empty session")
	}
	if !strings.Contains(err.Error(), "session required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

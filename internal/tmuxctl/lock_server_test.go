package tmuxctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestLockServer_HappyPath drives the controller end-to-end: create an
// anchor session, override its lock-command to a no-op (`true`) so tmux
// does not try to fork the default `lock -np` against a CI runner with
// no TTY, then call LockServer. tmux exits 0 because the iteration over
// attached clients is empty on a headless server — that is the contract
// every operator deployment relies on for the "secure every screen"
// primitive.
//
// The lock-command override is the load-bearing precondition: without
// it, a future tmux build that decides to invoke the lock command
// before iterating attached clients would surface a non-zero exit and
// false-flag the test. Pinning the lock-command to `true` keeps the
// behaviour deterministic regardless of which tmux happens to be on
// PATH.
func TestLockServer_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "ls_happy"
	if err := c.CreateSession(ctx, SessionSpec{
		Name: name, Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Override lock-command on the running server so the test passes
	// without a TTY. tmux's default `lock -np` would otherwise fork in
	// a CI environment that has no /dev/tty — even though the loop over
	// attached clients is empty on a headless server, future tmux
	// builds may still pre-spawn the lock binary, which would surface
	// as a confusing "lock: command not found" stderr instead of a
	// clean exit 0. Set it as a server-scoped option (`-g`) so it
	// applies to every client lock-server walks — lock-server iterates
	// every attached client, not the anchor session in particular.
	if _, err := c.run(ctx, "set-option", "-g", "lock-command", "true"); err != nil {
		t.Fatalf("set-option lock-command: %v", err)
	}

	if err := c.LockServer(ctx); err != nil {
		t.Fatalf("LockServer: %v", err)
	}
}

// TestLockServer_NoServerRunningWrapsSentinel pins the typed-error
// contract for "no daemon up": callers (and the JSON-RPC layer) must
// be able to errors.Is into errs.ErrSessionNotFound regardless of which
// exact phrase tmux emitted, so the dispatcher can map every "no server
// running" surface onto CodeSessionNotFound uniformly. Without that
// mapping, a fresh controller that called lock_server before any
// session was created would surface a generic CodeInternal error
// instead of the documented "target does not exist" code shared with
// list_clients / session_kill / lock_session.
//
// We deliberately do NOT spawn an anchor session here so the controller
// hits the "socket does not exist" path tmux emits as
// "error connecting to <socket> (No such file or directory)". The
// matcher in isNoServerRunningMsg covers both that phrasing and
// "no server running on <socket>" so the test stays insensitive to the
// exact spelling on whichever tmux ends up on PATH.
func TestLockServer_NoServerRunningWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.LockServer(ctx)
	if err == nil {
		t.Fatal("expected error for fresh controller with no server running")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestLockServer_Idempotent pins the back-to-back invariant: tmux's
// lock-server is itself a stateless iteration over attached clients,
// but the wrapper has its own error-mapping path (Controller.run) and
// a regression there would surface as a spurious failure on the second
// call — exactly the path agents whose lock hook fires twice (e.g.
// retried supervisor restarts) hit.
func TestLockServer_Idempotent(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "ls_idem", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.run(ctx, "set-option", "-g", "lock-command", "true"); err != nil {
		t.Fatalf("set-option lock-command: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := c.LockServer(ctx); err != nil {
			t.Fatalf("LockServer call %d: %v", i+1, err)
		}
	}
}

// TestLockServer_IsNoServerRunningMsg keeps the message-detector honest
// against the phrasings tmux actually emits. Drift here would silently
// turn the "no daemon" branch back into a generic CodeInternal error
// for every caller — same risk lock_client's matcher tests defend
// against for the client-not-found branch.
func TestLockServer_IsNoServerRunningMsg(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want bool
	}{
		{"no server running on /tmp/sock", true},
		{"NO SERVER RUNNING on /tmp/sock", true},
		{"error connecting to /tmp/sock (No such file or directory)", true},
		{"server exited unexpectedly", true},
		{"can't find session: foo", false},
		{"some unrelated stderr", false},
	}
	for _, tc := range cases {
		if got := isNoServerRunningMsg(tc.msg); got != tc.want {
			t.Errorf("isNoServerRunningMsg(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

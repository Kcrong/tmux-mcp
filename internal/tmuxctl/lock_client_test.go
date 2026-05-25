package tmuxctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestLockClient_NoClientsIsSuccessfulNoop is the load-bearing path
// for the headless servers tmux-mcp owns: with nothing attached,
// asking tmux to lock the "current" client is trivially a no-op, and
// the wrapper must surface it as nil rather than the raw "no current
// client" stderr. Without that mapping, every fire-and-forget lock
// on a headless server would have to first run list-clients to know
// whether to skip.
func TestLockClient_NoClientsIsSuccessfulNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the tmux server is up — lock-
	// client without a server returns a different error shape we
	// don't want to exercise here.
	if err := c.CreateSession(ctx, SessionSpec{Name: "lc_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.LockClient(ctx, ""); err != nil {
		t.Fatalf("LockClient empty client: %v", err)
	}
}

// TestLockClient_MissingClientWrapsSentinel pins the typed-error
// contract for an unknown client target: callers (and the JSON-RPC
// layer) must be able to errors.Is into errs.ErrSessionNotFound so
// the dispatcher can map the failure to CodeSessionNotFound uniformly
// with list_clients / session_kill.
func TestLockClient_MissingClientWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, named
	// client does not exist" (different stderr from "no server
	// running").
	if err := c.CreateSession(ctx, SessionSpec{Name: "lc_missing_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.LockClient(ctx, "/nonexistent_client_path_xyzzy")
	if err == nil {
		t.Fatal("expected error for missing client target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestLockClient_EmptyTargetUsesBareCommand documents the argv shape
// for the empty-target case — the wrapper must invoke tmux with a
// bare `lock-client` (no `-t` argument). On a headless server this
// path immediately hits the "no current client" branch, so the test
// pins the no-op observation rather than poking exec internals.
// Detecting drift here would otherwise require an injected exec
// recorder which the rest of the package deliberately avoids.
func TestLockClient_EmptyTargetUsesBareCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "lc_empty_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Two back-to-back calls verify the no-op surface stays clean
	// across repeated fire-and-forget lock requests — a regression
	// that left state behind would surface here as a non-nil error
	// on the second call.
	if err := c.LockClient(ctx, ""); err != nil {
		t.Fatalf("LockClient first empty: %v", err)
	}
	if err := c.LockClient(ctx, ""); err != nil {
		t.Fatalf("LockClient second empty: %v", err)
	}
}

// Predicate-level coverage for `isNoCurrentClientMsg` /
// `isClientMissingMsg` lives alongside their definitions in
// detach_client_test.go (TestDetachClient_IsNoCurrentClientMsg /
// TestDetachClient_IsClientMissingMsg). lock_client.go consumes the
// same package-level matchers, so duplicating the case tables here
// would only produce redundant failures on drift.

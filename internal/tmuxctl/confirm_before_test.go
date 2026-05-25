package tmuxctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestConfirmBefore_HeadlessWrapsSentinel pins the load-bearing
// failure shape for the headless tmux servers tmux-mcp owns: with no
// client attached, tmux has nothing to display the y/n prompt on, so
// confirm-before must surface a typed errs.ErrSessionNotFound rather
// than a successful no-op. Without that mapping an agent could
// believe a destructive command was queued behind a confirmation
// when in fact nobody ever saw the prompt — exactly the silent
// auto-execute behaviour the tool exists to prevent.
func TestConfirmBefore_HeadlessWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the tmux server is up — without
	// it confirm-before may surface the "no server running" branch
	// in a way that depends on whether anything has spun the server
	// yet. We want the "server up, no client attached" path here.
	if err := c.CreateSession(ctx, SessionSpec{Name: "cb_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// display-message is a harmless no-op command that proves the
	// argv shape works end-to-end without leaving any state behind
	// if a real client somehow accepts the prompt.
	err := c.ConfirmBefore(ctx, "go ahead?", "", "display-message ok")
	if err == nil {
		t.Fatal("expected error for headless confirm-before, got nil")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestConfirmBefore_MissingClientWrapsSentinel pins the typed-error
// contract for an unknown `-t <client>` target: callers (and the
// JSON-RPC layer) must be able to errors.Is into
// errs.ErrSessionNotFound so the dispatcher can map the failure to
// CodeSessionNotFound uniformly with list_clients / session_kill /
// lock_client.
func TestConfirmBefore_MissingClientWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "cb_missing_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.ConfirmBefore(ctx, "", "/nonexistent_client_path_xyzzy", "display-message ok")
	if err == nil {
		t.Fatal("expected error for missing client target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestConfirmBefore_RequiresCommand documents the contract that
// command is REQUIRED — tmux refuses confirm-before without one and
// the wrapper surfaces an explicit error rather than letting the
// generic tmux usage error propagate. An agent forgetting the field
// must learn from a clear "command required" rather than a confusing
// "usage: confirm-before …" line buried in stderr.
func TestConfirmBefore_RequiresCommand(t *testing.T) {
	t.Parallel()
	c := &Controller{} // no tmux invocation should reach here.
	err := c.ConfirmBefore(context.Background(), "ok?", "", "")
	if err == nil {
		t.Fatal("expected error for empty command, got nil")
	}
}

// TestConfirmBefore_IsNoServerOrClientMsg keeps the message-detector
// honest. The tmux phrasing for "no client to ask" comes in three
// shapes ("no current client", "no server running on …", "error
// connecting to …") and any of them must trip the wrap-into-sentinel
// branch. Drift here would silently turn the headless contract back
// into an opaque CodeInternal for every fire-and-forget caller.
func TestConfirmBefore_IsNoServerOrClientMsg(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want bool
	}{
		{"no current client", true},
		{"NO CURRENT CLIENT", true},
		{"tmux confirm-before: no current client", true},
		{"no server running on /tmp/tmux-1000/default", true},
		{"error connecting to /tmp/tmux-1000/default (No such file or directory)", true},
		{"can't find client: foo", false},
		{"some unrelated stderr", false},
	}
	for _, tc := range cases {
		if got := isNoServerOrClientMsg(tc.msg); got != tc.want {
			t.Errorf("isNoServerOrClientMsg(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

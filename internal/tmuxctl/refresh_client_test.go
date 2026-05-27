package tmuxctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestRefreshClient_NoClientsIsSuccessfulNoop is the load-bearing path
// for the headless servers tmux-mcp owns: with nothing attached, asking
// tmux to refresh "every client" is trivially a no-op, and the wrapper
// must surface it as nil rather than the raw "no current client" stderr.
// Without that mapping, every fire-and-forget refresh on a headless
// server would have to first run list-clients to know whether to skip.
func TestRefreshClient_NoClientsIsSuccessfulNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the tmux server is up — refresh-
	// client without a server returns a different error shape we don't
	// want to exercise here.
	if err := c.CreateSession(ctx, SessionSpec{Name: "rc_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.RefreshClient(ctx, "", false); err != nil {
		t.Fatalf("RefreshClient empty client: %v", err)
	}
	if err := c.RefreshClient(ctx, "", true); err != nil {
		t.Fatalf("RefreshClient empty client status-only: %v", err)
	}
}

// TestRefreshClient_MissingClientWrapsSentinel pins the typed-error
// contract for an unknown client target: callers (and the JSON-RPC
// layer) must be able to errors.Is into errs.ErrSessionNotFound so
// the dispatcher can map the failure to CodeSessionNotFound uniformly
// with list_clients / session_kill.
func TestRefreshClient_MissingClientWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, named
	// client does not exist" (different stderr from "no server
	// running").
	if err := c.CreateSession(ctx, SessionSpec{Name: "rc_missing_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.RefreshClient(ctx, "/nonexistent_client_path_xyzzy", false)
	if err == nil {
		t.Fatal("expected error for missing client target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestIsNoCurrentClientMsg keeps the message-detector honest. tmux's
// stderr phrasing has been stable for years but a future version that
// drifts on capitalisation must still trigger the no-op branch — drift
// here would silently turn the empty-client headless case back into a
// hard failure for every caller.
func TestIsNoCurrentClientMsg(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want bool
	}{
		{"no current client", true},
		{"NO CURRENT CLIENT", true},
		{"tmux refresh-client: no current client", true},
		{"can't find client: foo", false},
		{"some unrelated stderr", false},
	}
	for _, tc := range cases {
		if got := isNoCurrentClientMsg(tc.msg); got != tc.want {
			t.Errorf("isNoCurrentClientMsg(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// TestIsClientMissingMsg mirrors TestIsNoCurrentClientMsg for the
// "named client does not exist" branch — the predicate is the only
// thing standing between a real not-found error and a generic
// CodeInternal response, so the matcher needs to be loud about drift.
func TestIsClientMissingMsg(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want bool
	}{
		{"can't find client: /dev/pts/3", true},
		{"CAN'T FIND CLIENT: foo", true},
		{"no current client", false},
		{"some unrelated stderr", false},
	}
	for _, tc := range cases {
		if got := isClientMissingMsg(tc.msg); got != tc.want {
			t.Errorf("isClientMissingMsg(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

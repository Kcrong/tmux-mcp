package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestDetachClient_AllArgsEmptyRejected pins the controller-level
// validation contract: bare `DetachClient(ctx, "", "", false)` must
// fail up front rather than dispatching `tmux detach-client` (which
// targets the caller's "current" client — a concept that does not
// exist on the headless servers tmux-mcp owns). Without the up-front
// rejection a misbehaving caller would silently get tmux's "no
// current client" stderr instead of a clean validation error.
func TestDetachClient_AllArgsEmptyRejected(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.DetachClient(ctx, "", "", false)
	if err == nil {
		t.Fatal("expected error when client/session/all are all empty/false")
	}
	if !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("unexpected error phrasing: %v", err)
	}
}

// TestDetachClient_AllOnEmptyServerIsNoop is the load-bearing path for
// the headless servers tmux-mcp owns. With nothing attached, asking
// tmux to detach "every other client" (`-a`) is trivially a no-op,
// and the wrapper must surface it as nil rather than the raw "no
// current client" stderr. Without that mapping every fire-and-forget
// detach on a headless server would have to first run list-clients to
// know whether to skip.
func TestDetachClient_AllOnEmptyServerIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the tmux server is up — detach-
	// client without a server returns a different error shape we don't
	// want to exercise here.
	if err := c.CreateSession(ctx, SessionSpec{Name: "dc_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.DetachClient(ctx, "", "", true); err != nil {
		t.Fatalf("DetachClient all=true on empty server: %v", err)
	}
}

// TestDetachClient_BySessionWithNoAttachedIsNoop pins the by-session
// branch on the empty-roster path: scoping a detach to an existing
// session that has no attached clients must come back as a clean
// success rather than an error. The session resolves cleanly so tmux
// doesn't emit "can't find session"; instead it falls through to the
// "no current client" branch which the wrapper folds onto nil.
func TestDetachClient_BySessionWithNoAttachedIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "dc_sess", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.DetachClient(ctx, "", "dc_sess", false); err != nil {
		t.Fatalf("DetachClient by session on empty roster: %v", err)
	}
}

// TestDetachClient_MissingClientWrapsSentinel pins the typed-error
// contract for an unknown client target: callers (and the JSON-RPC
// layer) must be able to errors.Is into errs.ErrSessionNotFound so
// the dispatcher can map the failure to CodeSessionNotFound uniformly
// with list_clients / session_kill / refresh_client / lock_client.
func TestDetachClient_MissingClientWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, named
	// client does not exist" (different stderr from "no server
	// running").
	if err := c.CreateSession(ctx, SessionSpec{Name: "dc_missing_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.DetachClient(ctx, "/nonexistent_client_path_xyzzy", "", false)
	if err == nil {
		t.Fatal("expected error for missing client target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestDetachClient_MissingSessionIsNoop pins the by-session "no
// current client" fold. tmux's `detach-client -s GHOST` does NOT emit
// "can't find session" — instead it falls through to "no current
// client" (because once tmux resolves -s into a session it then looks
// for an attached client to detach, and zero matches read as "no
// current client" rather than "no such session"). The wrapper folds
// that exact stderr onto nil so a fire-and-forget detach against a
// typo'd or unattached session looks like a clean success rather than
// a sentinel error the caller must branch on.
//
// This is a behavioural contract: a missing session DOES NOT surface
// errs.ErrSessionNotFound for detach_client, unlike session_kill /
// list_clients. The asymmetry is deliberate and matches tmux's own
// argument resolution for `detach-client -s …`.
func TestDetachClient_MissingSessionIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the tmux server is up; the missing-
	// session path requires the daemon to be listening (otherwise tmux
	// emits "no server running" instead, which is a different code
	// path).
	if err := c.CreateSession(ctx, SessionSpec{Name: "dc_misses", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.DetachClient(ctx, "", "ghost_session_xyzzy", false); err != nil {
		t.Fatalf("DetachClient with missing session must be a no-op, got %v", err)
	}
}

// TestDetachClient_HappyPathSkipsWithoutAttachedClient covers the
// "real attached client" load-bearing path opportunistically. Spawning
// a tmux client requires a real PTY, which is fragile inside CI's
// hermetic sandbox; the conventional pattern (mirrored from
// refresh_client / lock_client tests) is to enumerate
// `list-clients` and skip when nothing is attached rather than fight
// the PTY. When a client is attached (e.g. a developer running this
// test from a real terminal that happens to share the controller's
// socket — which can't actually happen because every controller spins
// up its own private socket — but the pattern documents the intent),
// detach by name and re-enumerate to assert the named client
// disappears.
//
// In practice this test almost always hits the t.Skip branch in CI;
// keeping it here pins the calling shape so a future PTY-enabled
// runner exercises the by-client happy path without a code change.
func TestDetachClient_HappyPathSkipsWithoutAttachedClient(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "dc_happy", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	clients, lerr := c.ListClients(ctx, "")
	if lerr != nil {
		t.Fatalf("ListClients: %v", lerr)
	}
	if len(clients) == 0 {
		t.Skip("no attached tmux clients on controller socket; can't exercise happy detach without a real PTY")
	}
	target := clients[0].TTY
	if derr := c.DetachClient(ctx, target, "", false); derr != nil {
		t.Fatalf("DetachClient(%q): %v", target, derr)
	}
	after, aerr := c.ListClients(ctx, "")
	if aerr != nil {
		t.Fatalf("ListClients after detach: %v", aerr)
	}
	for _, ci := range after {
		if ci.TTY == target {
			t.Fatalf("client %q still present after DetachClient", target)
		}
	}
}

// TestDetachClient_IsNoCurrentClientMsg keeps the "nothing attached"
// matcher honest. tmux's stderr phrasing has been stable for years
// but a future version that drifts on capitalisation must still
// trigger the no-op branch — drift here would silently turn the
// fire-and-forget headless case back into a hard failure for every
// caller.
func TestDetachClient_IsNoCurrentClientMsg(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want bool
	}{
		{"no current client", true},
		{"NO CURRENT CLIENT", true},
		{"tmux detach-client: no current client", true},
		{"can't find client: foo", false},
		{"some unrelated stderr", false},
	}
	for _, tc := range cases {
		if got := isNoCurrentClientMsg(tc.msg); got != tc.want {
			t.Errorf("isNoCurrentClientMsg(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// TestDetachClient_IsClientMissingMsg mirrors the no-current-client
// matcher test for the "named client does not exist" branch — the
// predicate is the only thing standing between a real not-found error
// and a generic CodeInternal response, so the matcher needs to be
// loud about drift.
func TestDetachClient_IsClientMissingMsg(t *testing.T) {
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

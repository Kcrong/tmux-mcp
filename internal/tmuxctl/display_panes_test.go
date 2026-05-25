package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestDisplayPanes_NoAttachedClientIsNoop pins the load-bearing
// headless-server path: with nothing attached, tmux's `display-panes`
// returns non-zero with "no current client" stderr, and the wrapper
// must surface that as nil rather than the raw stderr. Without that
// fold every fire-and-forget display_panes on a headless server would
// have to first run list-clients to know whether to skip.
func TestDisplayPanes_NoAttachedClientIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the tmux server is up — display-
	// panes without a server returns a different error shape we don't
	// want to exercise here.
	if err := c.CreateSession(ctx, SessionSpec{Name: "dp_noop", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.DisplayPanes(ctx, DisplayPanesOpts{}); err != nil {
		t.Fatalf("DisplayPanes on empty server: %v", err)
	}
}

// TestDisplayPanes_NoAttachedClientWithTemplateIsNoop mirrors the
// no-client noop for the template-bearing variant. Forwarding a non-
// empty template must NOT change the headless-fold behaviour: tmux
// still reports "no current client" before it gets a chance to invoke
// the template, and the wrapper must keep folding that onto nil.
func TestDisplayPanes_NoAttachedClientWithTemplateIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "dp_noop_tpl", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.DisplayPanes(ctx, DisplayPanesOpts{
		Template: "select-pane -t %%",
	}); err != nil {
		t.Fatalf("DisplayPanes with template on empty server: %v", err)
	}
}

// TestDisplayPanes_MissingClientWrapsSentinel pins the typed-error
// contract for an unknown `-t` client target: callers (and the
// JSON-RPC layer) must be able to errors.Is into errs.ErrSessionNotFound
// so the dispatcher can map the failure to CodeSessionNotFound
// uniformly with list_clients / session_kill / detach_client.
func TestDisplayPanes_MissingClientWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, named
	// client does not exist" (different stderr from "no server
	// running").
	if err := c.CreateSession(ctx, SessionSpec{Name: "dp_missing_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.DisplayPanes(ctx, DisplayPanesOpts{
		Target: "/nonexistent_client_path_xyzzy",
	})
	if err == nil {
		t.Fatal("expected error for missing client target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestDisplayPanes_NegativeDurationRejected pins the up-front
// validation contract. tmux's `-d` happily accepts negative integers
// (silently treating them as "very long" on some versions), so the
// wrapper rejects negatives at the boundary before the call hits tmux.
// Without that pin a misbehaving caller could pin a live client in
// the picker for an extended period.
func TestDisplayPanes_NegativeDurationRejected(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.DisplayPanes(ctx, DisplayPanesOpts{
		Duration: -1 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected error for negative duration")
	}
	if !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("unexpected error phrasing: %v", err)
	}
}

// TestDisplayPanes_OversizedDurationRejected pins the upper-bound
// guard. The cap mirrors the JSON-RPC layer's maxDurationMs (10
// minutes) so a hostile caller cannot pin a tmux client in the picker
// for unbounded durations regardless of which boundary they hit.
func TestDisplayPanes_OversizedDurationRejected(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.DisplayPanes(ctx, DisplayPanesOpts{
		Duration: 11 * time.Minute,
	})
	if err == nil {
		t.Fatal("expected error for duration over cap")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unexpected error phrasing: %v", err)
	}
}

// TestDisplayPanes_HappyPathSkipsWithoutAttachedClient covers the
// "real attached client" path opportunistically. Spawning a tmux
// client requires a real PTY which is fragile inside CI; the
// conventional pattern (mirrored from refresh_client / lock_client /
// detach_client tests) is to enumerate `list-clients` and skip when
// nothing is attached rather than fight the PTY. Keeping the test
// here pins the calling shape so a future PTY-enabled runner exercises
// the happy path without a code change.
func TestDisplayPanes_HappyPathSkipsWithoutAttachedClient(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "dp_happy", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	clients, lerr := c.ListClients(ctx, "")
	if lerr != nil {
		t.Fatalf("ListClients: %v", lerr)
	}
	if len(clients) == 0 {
		t.Skip("no attached tmux clients on controller socket; can't exercise happy display_panes without a real PTY")
	}
	target := clients[0].TTY
	// duration is short so even a real PTY-bearing CI runner doesn't
	// stall the test on the 1s default display-panes-time. Using -b
	// would block until the user picks; we deliberately don't set it
	// here so the call returns as soon as the picker is drawn.
	if err := c.DisplayPanes(ctx, DisplayPanesOpts{
		Target:   target,
		Duration: 50 * time.Millisecond,
	}); err != nil {
		t.Fatalf("DisplayPanes(target=%q): %v", target, err)
	}
}

// TestDisplayPanes_IsNoClientMsg keeps the no-client matcher honest.
// tmux's stderr phrasing has been stable for years but a future
// version that drifts on capitalisation must still trigger the no-op
// branch — drift here would silently turn the fire-and-forget
// headless case back into a hard failure for every caller.
func TestDisplayPanes_IsNoClientMsg(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want bool
	}{
		{"no current client", true},
		{"NO CURRENT CLIENT", true},
		{"tmux display-panes: no current client", true},
		{"no current target", true},
		{"can't find client: foo", false},
		{"some unrelated stderr", false},
	}
	for _, tc := range cases {
		if got := isDisplayPanesNoClientMsg(tc.msg); got != tc.want {
			t.Errorf("isDisplayPanesNoClientMsg(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// TestDisplayPanes_IsClientMissingMsg mirrors the no-client matcher
// test for the "named client does not exist" branch — the predicate
// is the only thing standing between a real not-found error and a
// generic CodeInternal response, so the matcher needs to be loud
// about drift.
func TestDisplayPanes_IsClientMissingMsg(t *testing.T) {
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
		if got := isDisplayPanesClientMissingMsg(tc.msg); got != tc.want {
			t.Errorf("isDisplayPanesClientMissingMsg(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

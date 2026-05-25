package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSwitchClient_NoClientsIsSuccessfulNoop is the load-bearing path
// for the headless servers tmux-mcp owns: with nothing attached,
// asking tmux to switch the "current" client is trivially a no-op, and
// the wrapper must surface it as nil rather than the raw "no current
// client" stderr. Without that mapping, every fire-and-forget switch
// on a headless server would have to first run list-clients to know
// whether to skip.
func TestSwitchClient_NoClientsIsSuccessfulNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with two real sessions so the tmux server is up and there
	// is a legitimate target to switch to (an empty server would give
	// us the same no-op via a different stderr path we do not want to
	// exercise here).
	if err := c.CreateSession(ctx, SessionSpec{Name: "sc_a", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession sc_a: %v", err)
	}
	if err := c.CreateSession(ctx, SessionSpec{Name: "sc_b", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession sc_b: %v", err)
	}

	// target form, empty client → no current client → clean nil.
	if err := c.SwitchClient(ctx, "", "sc_b", false, false, false, false); err != nil {
		t.Fatalf("SwitchClient empty client target: %v", err)
	}
	// last form, empty client → same headless no-op.
	if err := c.SwitchClient(ctx, "", "", true, false, false, false); err != nil {
		t.Fatalf("SwitchClient empty client last: %v", err)
	}
	// readOnly toggle on top of a directional choice still no-ops on
	// a headless server — the toggle has nothing to apply to.
	if err := c.SwitchClient(ctx, "", "sc_a", false, false, false, true); err != nil {
		t.Fatalf("SwitchClient empty client readOnly: %v", err)
	}
}

// TestSwitchClient_NextPrevAreSuccessfulNoop covers the two
// directional flags symmetric to `last` so a future contributor adding
// a separate code path for either of them has to update the test in
// the same PR. tmux quietly ignores `-n`/`-p` on a server with no
// attached client, so the wrapper's success-mapping must apply
// uniformly across all three directional flags.
func TestSwitchClient_NextPrevAreSuccessfulNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "sc_n_a", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession sc_n_a: %v", err)
	}
	if err := c.CreateSession(ctx, SessionSpec{Name: "sc_n_b", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession sc_n_b: %v", err)
	}

	if err := c.SwitchClient(ctx, "", "", false, true, false, false); err != nil {
		t.Fatalf("SwitchClient next: %v", err)
	}
	if err := c.SwitchClient(ctx, "", "", false, false, true, false); err != nil {
		t.Fatalf("SwitchClient prev: %v", err)
	}
}

// TestSwitchClient_MissingTargetHeadlessNoop pins the empty-client
// behaviour for a headless server: tmux validates "no current client"
// before it ever resolves the target argument, so an unknown session
// passed as `-t TARGET` from an empty-client call still maps to a
// clean nil. Without that mapping every fire-and-forget redirect on a
// headless server would have to first run list-clients (and the
// missing-target branch would be unreachable from this codepath
// anyway).
//
// The "named target does not exist" sentinel-wrap path is exercised
// by the JSON-RPC handler test where we can name the client; here at
// the controller layer we only pin the headless contract.
func TestSwitchClient_MissingTargetHeadlessNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "sc_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.SwitchClient(ctx, "", "definitely_does_not_exist_xyzzy", false, false, false, false); err != nil {
		t.Fatalf("expected nil for headless empty-client missing-target call, got %v", err)
	}
}

// TestSwitchClient_MissingClientWrapsSentinel pins the same typed-error
// contract for a non-existent `client` argument. The matcher in this
// file (isSwitchClientMissingMsg) is the only thing standing between a
// genuine "named client does not exist" failure and a generic
// CodeInternal response.
func TestSwitchClient_MissingClientWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "sc_mc_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := c.CreateSession(ctx, SessionSpec{Name: "sc_mc_b", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.SwitchClient(ctx, "/dev/pts/_definitely_not_attached_xyzzy", "sc_mc_b",
		false, false, false, false)
	if err == nil {
		t.Fatal("expected error for missing client")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSwitchClient_RejectsZeroDirectional pins the exactly-one
// validation for the empty case: zero of {target, last, next, prev}
// must come back as a clean validation error before the wrapper ever
// shells out to tmux. Without this rule the daemon would emit "usage:"
// stderr and the failure would surface as a generic CodeInternal,
// which is hostile to clients trying to recover from a malformed call.
func TestSwitchClient_RejectsZeroDirectional(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.SwitchClient(ctx, "", "", false, false, false, false)
	if err == nil {
		t.Fatal("expected error when none of target/last/next/prev are set")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("error %q does not mention the exactly-one rule", err)
	}
}

// TestSwitchClient_RejectsTwoDirectional pins the inverse rule: any
// pair of {target, last, next, prev} must also be refused. We exercise
// both flag-vs-flag and target-vs-flag combinations because the
// validation paths are visually distinct in the wrapper (string check
// vs. boolean count) and a regression on either side would silently
// invite tmux to ignore one of the directives.
func TestSwitchClient_RejectsTwoDirectional(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	cases := []struct {
		name                       string
		target                     string
		last, next, prev, readOnly bool
	}{
		{"target+last", "sc_x", true, false, false, false},
		{"target+next", "sc_x", false, true, false, false},
		{"target+prev", "sc_x", false, false, true, false},
		{"last+next", "", true, true, false, false},
		{"last+prev", "", true, false, true, false},
		{"next+prev", "", false, true, true, false},
		{"all+readOnly", "sc_x", true, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.SwitchClient(ctx, "", tc.target, tc.last, tc.next, tc.prev, tc.readOnly)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), "mutually exclusive") {
				t.Fatalf("error %q does not mention mutual exclusion", err)
			}
		})
	}
}

// TestSwitchClient_IsSwitchNoCurrentClientMsg keeps the message
// detector honest. tmux's stderr phrasing has been stable for years
// but a future version that drifts on capitalisation must still
// trigger the no-op branch — drift here would silently turn the
// empty-client headless case back into a hard failure for every
// caller.
func TestSwitchClient_IsSwitchNoCurrentClientMsg(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want bool
	}{
		{"no current client", true},
		{"NO CURRENT CLIENT", true},
		{"tmux switch-client: no current client", true},
		{"can't find client: foo", false},
		{"some unrelated stderr", false},
	}
	for _, tc := range cases {
		if got := isSwitchNoCurrentClientMsg(tc.msg); got != tc.want {
			t.Errorf("isSwitchNoCurrentClientMsg(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// TestSwitchClient_IsSwitchClientMissingMsg mirrors the
// no-current-client matcher test for the "named client does not
// exist" branch — the predicate is the only thing standing between a
// real not-found error and a generic CodeInternal response, so the
// matcher needs to be loud about drift.
func TestSwitchClient_IsSwitchClientMissingMsg(t *testing.T) {
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
		if got := isSwitchClientMissingMsg(tc.msg); got != tc.want {
			t.Errorf("isSwitchClientMissingMsg(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

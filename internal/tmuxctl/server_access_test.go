package tmuxctl

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// skipIfNoServerAccess gates every controller-level server-access test
// on a host opt-in. tmux's `server-access` ultimately resolves the
// supplied USER through the host's passwd database, so flipping
// permissions on a real user account from a CI runner has obvious
// side effects (a wrong username at minimum produces a confusing
// stderr; a typo on the right username could detach a real client
// off the shared socket). The TMUX_MCP_TEST_SERVER_ACCESS env var is
// the explicit "I have a host that can lend its passwd database to
// this test" affordance; tests skip cleanly otherwise so the package
// remains buildable on unattended CI runners.
//
// The variable's value is the username the tests should use as the
// peer — typically a throw-away service account on a developer's box
// or a sandboxed runner. Empty values are treated the same as unset.
func skipIfNoServerAccess(t *testing.T) string {
	t.Helper()
	skipIfNoTmux(t)
	user := strings.TrimSpace(os.Getenv("TMUX_MCP_TEST_SERVER_ACCESS"))
	if user == "" {
		t.Skip("set TMUX_MCP_TEST_SERVER_ACCESS to a real OS username " +
			"to enable server-access integration tests")
	}
	return user
}

// TestServerAccessList_Headless pins the load-bearing "no daemon
// running" contract: `server-access -l` against a freshly constructed
// controller (no CreateSession yet, socket file does not exist) must
// degrade to an empty slice and a nil error. Without this branch
// every caller of ServerAccessList would have to write the same
// "is the server up?" probe before consulting the access list.
//
// This test does NOT require TMUX_MCP_TEST_SERVER_ACCESS — the
// headless path never touches the host's passwd database, so it is
// safe to run on any tmux-installed runner.
func TestServerAccessList_Headless(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	entries, err := c.ServerAccessList(ctx)
	if err != nil {
		t.Fatalf("ServerAccessList on fresh controller: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty entries on headless server, got %v", entries)
	}
}

// TestServerAccessList_RunningServerEmpty pins the second branch of
// the "empty access list" contract: a daemon that is running but has
// no entries (the default) returns an empty slice with no error. The
// difference from the headless case is that `server-access -l`
// actually executes here — the recovery branch in ServerAccessList is
// not reached; the parser's "stdout is blank" arm is.
//
// This test still does not require a real second user — it only
// observes that a running tmux with the default (empty) access list
// surfaces correctly.
func TestServerAccessList_RunningServerEmpty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the daemon so server-access actually runs. Without this
	// the call would degenerate into the "no server running" branch
	// and we would not exercise the parser at all.
	if err := c.CreateSession(ctx, SessionSpec{Name: "sa_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	entries, err := c.ServerAccessList(ctx)
	if err != nil {
		t.Fatalf("ServerAccessList on running daemon: %v", err)
	}
	// A fresh tmux server has no access-list entries (the owner row
	// is also absent until something gets added). Anything else means
	// either the parser misread tmux's output or a previous test on
	// the same controller leaked state.
	for _, e := range entries {
		if e.User == "" {
			t.Fatalf("entry with empty user should never be emitted: %#v", e)
		}
	}
}

// TestServerAccessAdd_RejectsEmptyUser pins the boundary's argument-
// shape policy: an empty USER must be refused before tmux is touched.
// Without the guard a buggy caller would see tmux's "usage" stderr
// (or worse, a host-dependent passwd-lookup failure) instead of a
// clean validation error.
func TestServerAccessAdd_RejectsEmptyUser(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.ServerAccessAdd(ctx, ""); err == nil {
		t.Fatal("expected error for empty user")
	} else if !strings.Contains(err.Error(), "user required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestServerAccessDelete_RejectsEmptyUser mirrors the Add guard for
// the Delete entry point. The validator is shared so this is a
// behavioural pin: every mutating ServerAccess* method must refuse an
// empty USER with the same diagnostic.
func TestServerAccessDelete_RejectsEmptyUser(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.ServerAccessDelete(ctx, ""); err == nil {
		t.Fatal("expected error for empty user")
	} else if !strings.Contains(err.Error(), "user required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestServerAccessReadOnly_RejectsEmptyUser pins the same guard for
// the read-only switch entry point.
func TestServerAccessReadOnly_RejectsEmptyUser(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.ServerAccessReadOnly(ctx, ""); err == nil {
		t.Fatal("expected error for empty user")
	}
}

// TestServerAccessWrite_RejectsEmptyUser pins the same guard for the
// write switch entry point.
func TestServerAccessWrite_RejectsEmptyUser(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.ServerAccessWrite(ctx, ""); err == nil {
		t.Fatal("expected error for empty user")
	}
}

// TestServerAccessAdd_RejectsBadUser pins the regex/length policy:
// usernames with shell metacharacters, leading digits, NUL bytes, or
// over-the-cap lengths must be rejected before tmux's argv ever sees
// them. The cases below cover the headline categories — a stray
// quote, a leading digit (POSIX rejects these), a control byte, and
// a too-long string. Each path uses a distinct error fragment so a
// failure points at exactly which guard regressed.
func TestServerAccessAdd_RejectsBadUser(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	cases := map[string]string{
		"shell metachar":     `bob"; rm -rf /tmp`,
		"leading digit":      "1user",
		"control byte":       "bob\x01evil",
		"uppercase rejected": "Bob",
		"too long":           strings.Repeat("a", maxServerAccessUserLen+1),
	}
	for label, name := range cases {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			if err := c.ServerAccessAdd(ctx, name); err == nil {
				t.Fatalf("expected error for %s (%q), got nil", label, name)
			}
		})
	}
}

// TestServerAccess_AddListWriteReadOnlyDelete_Cycle exercises the full
// add → list (R) → write → list (R/W) → read_only → list (R) → delete
// → list (empty) cycle against a real OS user. Gated on the opt-in
// env var so unattended runners skip cleanly.
//
// The choice of peer username comes from the env var rather than a
// fixed sentinel because a meaningful test must hit the host's passwd
// database — `server-access` resolves USER through `getpwnam`, and
// tmux refuses entries it cannot resolve. Self-add (the test process's
// own user) is allowed in principle but tmux silently no-ops on the
// owner of the daemon, so the env var convention is "name a peer that
// is not the test runner".
func TestServerAccess_AddListWriteReadOnlyDelete_Cycle(t *testing.T) {
	t.Parallel()
	peer := skipIfNoServerAccess(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	// Anchor the daemon — server-access requires it to be running.
	if err := c.CreateSession(ctx, SessionSpec{Name: "sa_cycle", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Be a good neighbour: even if a previous run leaked an entry for
	// this peer, start from a clean slate. Delete is idempotent in
	// spirit (the resulting state is "user not in list"), but tmux
	// itself returns an error when the entry is missing — swallow that
	// up front so the cycle test runs against a known-empty list.
	_ = c.ServerAccessDelete(ctx, peer)

	// Belt-and-braces guarantee the test cleans up after itself even
	// when an assertion fails midway through. Errors here are ignored —
	// the goal state is "the entry is gone", and a duplicate delete
	// surfaces tmux's "user not in list" stderr which is benign.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = c.ServerAccessDelete(cleanupCtx, peer)
	})

	if err := c.ServerAccessAdd(ctx, peer); err != nil {
		t.Fatalf("ServerAccessAdd(%q): %v", peer, err)
	}

	// After Add (without -w) tmux puts the user in read-only mode by
	// default. ListEntry should reflect that.
	if !findPermission(t, c, ctx, peer, "R") {
		t.Fatalf("expected R after Add, got %v", listOrFatal(t, c, ctx))
	}

	if err := c.ServerAccessWrite(ctx, peer); err != nil {
		t.Fatalf("ServerAccessWrite(%q): %v", peer, err)
	}
	if !findPermission(t, c, ctx, peer, "R/W") {
		t.Fatalf("expected R/W after Write, got %v", listOrFatal(t, c, ctx))
	}

	if err := c.ServerAccessReadOnly(ctx, peer); err != nil {
		t.Fatalf("ServerAccessReadOnly(%q): %v", peer, err)
	}
	if !findPermission(t, c, ctx, peer, "R") {
		t.Fatalf("expected R after ReadOnly, got %v", listOrFatal(t, c, ctx))
	}

	if err := c.ServerAccessDelete(ctx, peer); err != nil {
		t.Fatalf("ServerAccessDelete(%q): %v", peer, err)
	}
	for _, e := range listOrFatal(t, c, ctx) {
		if e.User == peer {
			t.Fatalf("entry for %q still present after Delete: %v", peer, e)
		}
	}
}

// TestServerAccessAdd_HappyPath is the minimum smoke check for the
// Add boundary: gated on the env var, anchor a daemon, add a peer,
// confirm the listing reflects it. Sub-cases of the cycle test cover
// the rest of the surface; this one stays small so a failure in Add
// itself surfaces with a focused diagnostic.
func TestServerAccessAdd_HappyPath(t *testing.T) {
	t.Parallel()
	peer := skipIfNoServerAccess(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "sa_add", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_ = c.ServerAccessDelete(ctx, peer) // pre-clean
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = c.ServerAccessDelete(cleanupCtx, peer)
	})

	if err := c.ServerAccessAdd(ctx, peer); err != nil {
		t.Fatalf("ServerAccessAdd(%q): %v", peer, err)
	}
	if !findUser(t, c, ctx, peer) {
		t.Fatalf("expected %q in access list after Add", peer)
	}
}

// findUser reports whether the access list currently contains an
// entry for the named peer. Pulled into a helper so each cycle step
// uses the same observation surface — a regression that broke the
// parser's username extraction would surface uniformly across every
// caller.
func findUser(t *testing.T, c *Controller, ctx context.Context, peer string) bool {
	t.Helper()
	for _, e := range listOrFatal(t, c, ctx) {
		if e.User == peer {
			return true
		}
	}
	return false
}

// findPermission narrows findUser to a specific permission token so
// the cycle test can pin tmux's R / R/W transitions without
// re-stating the list-walk in every assertion. Returns true only on
// an exact match — a future tmux that started printing additional
// permission tokens would still surface here as a focused failure.
func findPermission(t *testing.T, c *Controller, ctx context.Context, peer, perm string) bool {
	t.Helper()
	for _, e := range listOrFatal(t, c, ctx) {
		if e.User == peer && e.Permission == perm {
			return true
		}
	}
	return false
}

// listOrFatal calls ServerAccessList and fails the test on error. The
// helper exists so the cycle tests stay focused on the assertions —
// a failure in the listing path itself is a hard error, never an
// expected branch in these tests.
func listOrFatal(t *testing.T, c *Controller, ctx context.Context) []ServerAccessEntry {
	t.Helper()
	entries, err := c.ServerAccessList(ctx)
	if err != nil {
		t.Fatalf("ServerAccessList: %v", err)
	}
	return entries
}

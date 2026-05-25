package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// hookSet polls tmux for a per-session hook entry naming the requested
// event. Session-scoped hooks land in two namespaces depending on the
// event class:
//
//   - Window-class events (e.g. `pane-died`, `alert-activity`) appear
//     under `show-options -t SESSION -wH` because tmux files them in
//     the session's window-options table.
//   - Server / session-class events (e.g. `client-attached`,
//     `session-created`) appear under `show-options -t SESSION -H`
//     because tmux files them in the session's session-options table.
//
// We probe both and report a hit if either lists `name[idx] ...` —
// the literal `[` discriminator keeps the substring match from
// accidentally firing on a similarly-named option that just happens
// to share a prefix with the hook name.
func hookSet(t *testing.T, c *Controller, target, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	needle := name + "["
	for _, args := range [][]string{
		{"show-options", "-t", target, "-wH"},
		{"show-options", "-t", target, "-H"},
	} {
		out, err := c.run(ctx, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), needle) {
				return true
			}
		}
	}
	return false
}

// hookSetGlobal polls tmux for a global hook entry naming the
// requested event. Global hooks land in two namespaces depending on
// the event class:
//
//   - Window-class events (e.g. `pane-died`, `alert-activity`) appear
//     under `show-options -gwH` because tmux files them in the
//     server's global window-options table.
//   - Server / session-class events (e.g. `client-attached`,
//     `session-created`) appear under `show-options -gH` because tmux
//     files them in the server's global session-options table.
//
// We probe both and report a hit if either lists `name[idx] ...` —
// the literal `[` discriminator keeps the substring match from
// accidentally firing on a similarly-named option that just happens
// to share a prefix with the hook name.
func hookSetGlobal(t *testing.T, c *Controller, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	needle := name + "["
	for _, args := range [][]string{
		{"show-options", "-gwH"},
		{"show-options", "-gH"},
	} {
		out, err := c.run(ctx, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), needle) {
				return true
			}
		}
	}
	return false
}

// TestSetHook_BindAndUnset is the load-bearing happy-path test: bind a
// no-op hook to a session, verify it shows up in show-options, unset
// it, verify it's gone. Every agent reaching for set_hook to wire up
// pane-died / client-attached handlers depends on this contract.
func TestSetHook_BindAndUnset(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "hk", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.SetHook(ctx, "pane-died", `display-message "ping"`, "hk", false, false); err != nil {
		t.Fatalf("SetHook bind: %v", err)
	}
	if !hookSet(t, c, "hk", "pane-died") {
		t.Fatal("expected pane-died hook to be set on session hk after bind")
	}

	if err := c.SetHook(ctx, "pane-died", "", "hk", true, false); err != nil {
		t.Fatalf("SetHook unset: %v", err)
	}
	if hookSet(t, c, "hk", "pane-died") {
		t.Fatal("expected pane-died hook to be cleared on session hk after unset")
	}
}

// TestSetHook_Global pins the -g (server-wide) bind path. Hooks set
// with -g land on the server's global options table, accessible to
// every current and future session — exactly the surface a
// "react to every client-attached" supervisor wants.
func TestSetHook_Global(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the daemon is up; -g works without
	// one but tying tests to the daemon-already-running path keeps the
	// expected stderr stable across tmux versions.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession anchor: %v", err)
	}

	if err := c.SetHook(ctx, "client-attached", `display-message "global"`, "", false, true); err != nil {
		t.Fatalf("SetHook -g bind: %v", err)
	}
	if !hookSetGlobal(t, c, "client-attached") {
		t.Fatal("expected client-attached hook to be set globally after -g bind")
	}

	if err := c.SetHook(ctx, "client-attached", "", "", true, true); err != nil {
		t.Fatalf("SetHook -g unset: %v", err)
	}
	if hookSetGlobal(t, c, "client-attached") {
		t.Fatal("expected client-attached hook to be cleared globally after -g unset")
	}
}

// TestSetHook_MissingSessionWrapsSentinel pins the typed-error contract
// for an unknown target session: callers (and the JSON-RPC layer) must
// be able to errors.Is into errs.ErrSessionNotFound regardless of which
// exact phrase tmux emitted ("can't find session" vs "session not
// found").
func TestSetHook_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, session
	// missing" rather than "no server" (which surfaces a different
	// stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.SetHook(ctx, "pane-died", `display-message "x"`, "ghost_session_xyzzy", false, false)
	if err == nil {
		t.Fatal("expected error for missing target session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSetHook_RejectsEmptyName locks the up-front guard. tmux would
// otherwise emit a generic "too few arguments" stderr the caller would
// have to substring-match.
func TestSetHook_RejectsEmptyName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.SetHook(ctx, "", `display-message "x"`, "any", false, false)
	if err == nil {
		t.Fatal("expected error for empty hook name")
	}
	if !strings.Contains(err.Error(), "hook name required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSetHook_RejectsEmptyCommandOnBind locks the bind-path guard. A
// non-unset call without a command would otherwise reach tmux with a
// trailing empty positional argument, which is rejected with a less
// helpful stderr template.
func TestSetHook_RejectsEmptyCommandOnBind(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.SetHook(ctx, "pane-died", "", "any", false, false)
	if err == nil {
		t.Fatal("expected error for empty command on bind path")
	}
	if !strings.Contains(err.Error(), "hook command required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSetHook_RejectsEmptyTargetWhenNotGlobal pins the per-session
// bind-path guard. Without the up-front check tmux would resolve "" to
// whatever session it considered current, silently mis-routing the
// hook against a stale target.
func TestSetHook_RejectsEmptyTargetWhenNotGlobal(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.SetHook(ctx, "pane-died", `display-message "x"`, "", false, false)
	if err == nil {
		t.Fatal("expected error for empty target when not global")
	}
	if !strings.Contains(err.Error(), "hook target required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSetHook_UnsetIdempotent confirms unsetting a hook that was never
// set is not a hard failure — tmux's `set-hook -u` is content with the
// no-op when the hook is absent. This matters for deployment scripts
// that re-run their teardown unconditionally.
func TestSetHook_UnsetIdempotent(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "idem", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// First unset on a hook that was never set: no error.
	if err := c.SetHook(ctx, "pane-died", "", "idem", true, false); err != nil {
		t.Fatalf("SetHook unset (no-op): %v", err)
	}
	// Bind, then unset twice in a row.
	if err := c.SetHook(ctx, "pane-died", `display-message "x"`, "idem", false, false); err != nil {
		t.Fatalf("SetHook bind: %v", err)
	}
	if err := c.SetHook(ctx, "pane-died", "", "idem", true, false); err != nil {
		t.Fatalf("SetHook unset 1: %v", err)
	}
	if err := c.SetHook(ctx, "pane-died", "", "idem", true, false); err != nil {
		t.Fatalf("SetHook unset 2 (no-op): %v", err)
	}
	if hookSet(t, c, "idem", "pane-died") {
		t.Fatal("expected pane-died hook to be cleared after repeated unset")
	}
}

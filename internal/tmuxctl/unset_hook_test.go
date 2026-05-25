package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// hookSetSession polls tmux for a per-session hook entry naming the
// requested event. Session-scoped hooks land in two namespaces depending
// on the event class:
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
func hookSetSession(t *testing.T, c *Controller, target, name string) bool {
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

// hookSetGlobalScope polls tmux for a global hook entry naming the
// requested event. Global hooks land in two namespaces depending on
// the event class:
//
//   - Window-class events appear under `show-options -gwH` (global
//     window-options table).
//   - Server / session-class events appear under `show-options -gH`
//     (global session-options table).
//
// Same `name[` discriminator as hookSetSession.
func hookSetGlobalScope(t *testing.T, c *Controller, name string) bool {
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

// hookSetWindowScope polls tmux for a window-scoped hook entry naming
// the requested event. `-w -t target` honours both the per-window and
// per-session window-options tables, so probing under `show-options
// -t SESSION -wH` is enough — tmux walks the same lookup chain at
// `set-hook -w` time.
func hookSetWindowScope(t *testing.T, c *Controller, target, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	needle := name + "["
	out, err := c.run(ctx, "show-options", "-t", target, "-wH")
	if err != nil {
		t.Fatalf("show-options -t %s -wH: %v", target, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), needle) {
			return true
		}
	}
	return false
}

// TestUnsetHook_SessionRoundTrip is the load-bearing happy path: bind
// a no-op hook to a session via `set-hook -t`, observe it in
// show-options, ask UnsetHook to clear it, observe it is gone. A
// regression where the boundary dropped `-t TARGET` from the unset
// argv would either no-op (the bind is in a session we didn't target)
// or wipe the wrong scope — both surface as the post-unset probe still
// finding the hook.
func TestUnsetHook_SessionRoundTrip(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "uh", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Bind via the raw set-hook path so this suite does not depend on
	// the not-yet-merged SetHook controller method.
	if _, err := c.run(ctx, "set-hook", "-t", "uh", "pane-died", `display-message "ping"`); err != nil {
		t.Fatalf("set-hook bind: %v", err)
	}
	if !hookSetSession(t, c, "uh", "pane-died") {
		t.Fatalf("pre-condition: pane-died hook not present after bind")
	}

	if err := c.UnsetHook(ctx, "uh", "pane-died", false, false); err != nil {
		t.Fatalf("UnsetHook session: %v", err)
	}
	if hookSetSession(t, c, "uh", "pane-died") {
		t.Fatal("expected pane-died hook to be cleared on session uh after UnsetHook")
	}
}

// TestUnsetHook_GlobalRoundTrip pins the `-g` (server-wide) clear
// path. Global hooks land on the server's global options table,
// accessible to every current and future session — exactly the
// surface a "react to every client-attached" supervisor wants to
// tear down with one call. A regression where the boundary forgot
// to forward `-g` would clear the per-session table instead, which
// surfaces as the post-unset probe still finding the global entry.
func TestUnsetHook_GlobalRoundTrip(t *testing.T) {
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

	if _, err := c.run(ctx, "set-hook", "-g", "client-attached", `display-message "global"`); err != nil {
		t.Fatalf("set-hook -g bind: %v", err)
	}
	if !hookSetGlobalScope(t, c, "client-attached") {
		t.Fatal("pre-condition: client-attached hook not present after global bind")
	}

	if err := c.UnsetHook(ctx, "", "client-attached", true, false); err != nil {
		t.Fatalf("UnsetHook -g: %v", err)
	}
	if hookSetGlobalScope(t, c, "client-attached") {
		t.Fatal("expected client-attached hook to be cleared globally after UnsetHook -g")
	}
}

// TestUnsetHook_WindowRoundTrip pins the `-w` (window-scoped) clear
// path. Window-class events live in the window-options table; the
// `-w` flag flips the unset to clear there. We bind via `set-hook -w
// -t SESSION` so the suite probes the same lookup chain UnsetHook
// targets, then ask UnsetHook(global=false, window=true) to clear it.
// A regression where the boundary swapped `-w` for `-g` (or dropped
// the flag entirely) would leave the binding in place.
func TestUnsetHook_WindowRoundTrip(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "uw", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := c.run(ctx, "set-hook", "-w", "-t", "uw", "pane-died", `display-message "win"`); err != nil {
		t.Fatalf("set-hook -w bind: %v", err)
	}
	if !hookSetWindowScope(t, c, "uw", "pane-died") {
		t.Fatal("pre-condition: pane-died hook not present after window bind")
	}

	if err := c.UnsetHook(ctx, "uw", "pane-died", false, true); err != nil {
		t.Fatalf("UnsetHook -w: %v", err)
	}
	if hookSetWindowScope(t, c, "uw", "pane-died") {
		t.Fatal("expected pane-died hook to be cleared on window after UnsetHook -w")
	}
}

// TestUnsetHook_RejectsEmptyName locks the up-front guard. tmux would
// otherwise emit a generic "too few arguments" stderr the caller would
// have to substring-match. Mirror's set_hook's contract so callers
// can errors.Is / substring-match the same shape on either tool.
func TestUnsetHook_RejectsEmptyName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.UnsetHook(ctx, "any", "", false, false)
	if err == nil {
		t.Fatal("expected error for empty hook name")
	}
	if !strings.Contains(err.Error(), "hook name required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestUnsetHook_RejectsBothGlobalAndWindow pins the mutual-exclusion
// guard. tmux's `-g` and `-w` are mutually exclusive on the unset
// path; without the up-front check tmux would resolve the contradiction
// silently in a version-dependent way (older tmux walks the args left
// to right and uses whichever wins; newer tmux emits a different
// stderr template). Refusing the shape here keeps the boundary
// from inheriting that footgun.
func TestUnsetHook_RejectsBothGlobalAndWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.UnsetHook(ctx, "", "pane-died", true, true)
	if err == nil {
		t.Fatal("expected error for global=true + window=true")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestUnsetHook_HeadlessWrapsSessionNotFound pins the typed-error
// contract on the headless path: a per-session unset call (neither
// `-g` nor `-w`) without a target must surface as
// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
// CodeSessionNotFound (-32000). Without this, a malformed call would
// surface tmux's "no current target" stderr — the version-dependent
// shape callers would have to substring-match.
func TestUnsetHook_HeadlessWrapsSessionNotFound(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.UnsetHook(ctx, "", "pane-died", false, false)
	if err == nil {
		t.Fatal("expected error for empty target on per-session unset")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestUnsetHook_MissingSessionWrapsSentinel pins the typed-error
// contract for an unknown target session: callers (and the JSON-RPC
// layer) must be able to errors.Is into errs.ErrSessionNotFound
// regardless of which exact phrase tmux emitted ("can't find session"
// vs "session not found").
func TestUnsetHook_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, session
	// missing" rather than "no server" (which surfaces a different
	// stderr shape, also folded into the same sentinel).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.UnsetHook(ctx, "ghost_session_xyzzy", "pane-died", false, false)
	if err == nil {
		t.Fatal("expected error for missing target session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestUnsetHook_IdempotentMissingHook confirms unsetting a hook that
// was never set is not a hard failure — tmux's `set-hook -u` is
// content with the no-op when the hook is absent. This matters for
// deployment scripts that re-run their teardown unconditionally.
func TestUnsetHook_IdempotentMissingHook(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "idem", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// First unset on a hook that was never set: no error.
	if err := c.UnsetHook(ctx, "idem", "pane-died", false, false); err != nil {
		t.Fatalf("UnsetHook (no-op): %v", err)
	}
	// Bind, then unset twice in a row.
	if _, err := c.run(ctx, "set-hook", "-t", "idem", "pane-died", `display-message "x"`); err != nil {
		t.Fatalf("set-hook bind: %v", err)
	}
	if err := c.UnsetHook(ctx, "idem", "pane-died", false, false); err != nil {
		t.Fatalf("UnsetHook 1: %v", err)
	}
	if err := c.UnsetHook(ctx, "idem", "pane-died", false, false); err != nil {
		t.Fatalf("UnsetHook 2 (no-op): %v", err)
	}
	if hookSetSession(t, c, "idem", "pane-died") {
		t.Fatal("expected pane-died hook to be cleared after repeated unset")
	}
}

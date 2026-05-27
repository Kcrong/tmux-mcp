package tmuxctl

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// findHookEntry returns the first entry whose Name + Target match,
// useful when a probe response interleaves multiple hooks and the test
// only cares about pinning one of them.
func findHookEntry(t *testing.T, hooks []HookEntry, name, target string) (HookEntry, bool) {
	t.Helper()
	for _, h := range hooks {
		if h.Name == name && h.Target == target {
			return h, true
		}
	}
	return HookEntry{}, false
}

// TestShowHooks_GlobalRoundTrip is the load-bearing happy path: install
// a server-global hook with set-hook, call ShowHooks(target=""), and
// verify the binding round-trips. The test pins the full triple
// (Name, Command, Target) so a parser regression that drops the
// command body or leaks the `[idx]` suffix into the name fails here.
func TestShowHooks_GlobalRoundTrip(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the daemon so show-options against a global table works
	// against a live server (otherwise we exercise the empty-server
	// branch, which is its own test below).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// pane-died is a window-class hook — it lands in the global
	// window-options table (`-gwH`), so this exercises the
	// showHooksGlobal helper's window probe. The body embeds a space
	// so tmux preserves the surrounding quotes when echoing back —
	// turns the assertion below into an exact-match check that pins
	// both "body survived parsing" AND "parser kept embedded space".
	const cmd = `display-message "pane has died"`
	if _, err := c.run(ctx, "set-hook", "-g", "pane-died", cmd); err != nil {
		t.Fatalf("set-hook -g pane-died: %v", err)
	}

	hooks, err := c.ShowHooks(ctx, "")
	if err != nil {
		t.Fatalf("ShowHooks: %v", err)
	}
	got, ok := findHookEntry(t, hooks, "pane-died", "")
	if !ok {
		t.Fatalf("expected pane-died hook in global scope; got %v", hooks)
	}
	if got.Command != cmd {
		t.Fatalf("Command mismatch: got %q, want %q", got.Command, cmd)
	}
	if got.Target != "" {
		t.Fatalf("Target = %q, want \"\" for global hook", got.Target)
	}
}

// TestShowHooks_GlobalServerClass pins the -gH path (server/session-class
// hooks like client-attached). A regression that called only -gwH
// would leak this hook out of the response.
func TestShowHooks_GlobalServerClass(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Use a body with embedded spaces so tmux preserves the surrounding
	// quotes when echoing it back; that turns the assertion into an
	// exact-match (rather than the substring check the simpler
	// pane-died test uses) so a parser that trimmed surrounding
	// quotes would fail here.
	const cmd = `display-message "client attached"`
	if _, err := c.run(ctx, "set-hook", "-g", "client-attached", cmd); err != nil {
		t.Fatalf("set-hook -g client-attached: %v", err)
	}

	hooks, err := c.ShowHooks(ctx, "")
	if err != nil {
		t.Fatalf("ShowHooks: %v", err)
	}
	got, ok := findHookEntry(t, hooks, "client-attached", "")
	if !ok {
		t.Fatalf("expected client-attached hook in global scope; got %v", hooks)
	}
	if got.Command != cmd {
		t.Fatalf("Command mismatch: got %q, want %q", got.Command, cmd)
	}
}

// TestShowHooks_PerSessionScoped pins the target!="" branch: a hook
// installed against a specific session must show up with Target == that
// session, and ShowHooks(targetX) must return ONLY hooks scoped to
// targetX (not the global ones, not other sessions').
func TestShowHooks_PerSessionScoped(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "alpha", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession alpha: %v", err)
	}
	if err := c.CreateSession(ctx, SessionSpec{Name: "beta", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession beta: %v", err)
	}

	// Bind one hook to alpha, a different one to beta. The bodies
	// embed a space so tmux preserves the surrounding quotes when
	// echoing them back, turning the assertion below into an
	// exact-match check.
	if _, err := c.run(ctx, "set-hook", "-t", "alpha", "alert-bell", `display-message "alpha bell"`); err != nil {
		t.Fatalf("set-hook alpha: %v", err)
	}
	if _, err := c.run(ctx, "set-hook", "-t", "beta", "alert-bell", `display-message "beta bell"`); err != nil {
		t.Fatalf("set-hook beta: %v", err)
	}

	hooks, err := c.ShowHooks(ctx, "alpha")
	if err != nil {
		t.Fatalf("ShowHooks alpha: %v", err)
	}
	got, ok := findHookEntry(t, hooks, "alert-bell", "alpha")
	if !ok {
		t.Fatalf("expected alert-bell hook on alpha; got %v", hooks)
	}
	if got.Command != `display-message "alpha bell"` {
		t.Fatalf("alpha command = %q; want display-message \"alpha bell\"", got.Command)
	}
	// And the cross-session bleed check: ShowHooks("alpha") must NOT
	// return beta's hook.
	if _, ok := findHookEntry(t, hooks, "alert-bell", "beta"); ok {
		t.Fatalf("ShowHooks(alpha) leaked beta's hook: %v", hooks)
	}
}

// TestShowHooks_GlobalScanIncludesSessions pins the target=="" sweep:
// when no target is supplied, the response must include both the
// global bindings AND every session's per-session bindings, so an
// operator dump has the full picture in one call.
func TestShowHooks_GlobalScanIncludesSessions(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "sweep", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// One global + one session-scoped hook so the assertion sees both.
	if _, err := c.run(ctx, "set-hook", "-g", "client-attached", `display-message "g"`); err != nil {
		t.Fatalf("set-hook -g: %v", err)
	}
	if _, err := c.run(ctx, "set-hook", "-t", "sweep", "alert-activity", `display-message "s"`); err != nil {
		t.Fatalf("set-hook -t: %v", err)
	}

	hooks, err := c.ShowHooks(ctx, "")
	if err != nil {
		t.Fatalf("ShowHooks: %v", err)
	}
	if _, ok := findHookEntry(t, hooks, "client-attached", ""); !ok {
		t.Fatalf("global client-attached missing from sweep: %v", hooks)
	}
	if _, ok := findHookEntry(t, hooks, "alert-activity", "sweep"); !ok {
		t.Fatalf("per-session alert-activity missing from sweep: %v", hooks)
	}
}

// TestShowHooks_EmptyServerReturnsEmpty pins the cold-start case: a
// fresh controller whose tmux server has not been spawned must answer
// with an empty slice — never nil — and never an error. This is the
// shape JSON encoding leans on so a `{"hooks": []}` response is the
// load-bearing default.
func TestShowHooks_EmptyServerReturnsEmpty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	hooks, err := c.ShowHooks(ctx, "")
	if err != nil {
		t.Fatalf("ShowHooks on empty server: %v", err)
	}
	if hooks == nil {
		t.Fatal("hooks must be empty []HookEntry, never nil")
	}
	if len(hooks) != 0 {
		t.Fatalf("expected zero hooks on empty server, got %d: %v", len(hooks), hooks)
	}
}

// TestShowHooks_NoHooksReturnsEmpty exercises the "server is up, but
// the operator never bound anything" path. The tmux daemon is running
// (anchor session), no set-hook calls have been issued, and the
// response must still be a non-nil empty slice.
func TestShowHooks_NoHooksReturnsEmpty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "noh", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	hooks, err := c.ShowHooks(ctx, "")
	if err != nil {
		t.Fatalf("ShowHooks: %v", err)
	}
	if hooks == nil {
		t.Fatal("hooks must be empty []HookEntry, never nil")
	}
	if len(hooks) != 0 {
		t.Fatalf("expected zero hooks on a server with no bindings, got %d: %v",
			len(hooks), hooks)
	}
}

// TestShowHooks_MissingSessionWrapsSentinel pins the typed-error
// contract for an unknown target session: callers (and the JSON-RPC
// layer) must be able to errors.Is the result into ErrSessionNotFound
// regardless of which exact phrase tmux emitted.
func TestShowHooks_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the server is up — we exercise
	// "server up, target missing" rather than the "no server" branch.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err := c.ShowHooks(ctx, "ghost_session_xyzzy")
	if err == nil {
		t.Fatal("expected error for missing target session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestShowHooks_RoundTripPreservesQuotes pins the parser's no-touch
// promise on the command body: when tmux preserves the surrounding
// quotes (because the body contains whitespace), the captured string
// must equal what set-hook installed verbatim. A regression that
// re-quoted or trimmed the body would silently corrupt agent configs
// that rely on the exact display form. tmux normalises quoting only
// when it is non-essential (single arg with no whitespace), so a body
// with embedded spaces is the load-bearing fixture.
func TestShowHooks_RoundTripPreservesQuotes(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "quote", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	const cmd = `display-message "x with spaces"`
	if _, err := c.run(ctx, "set-hook", "-t", "quote", "alert-bell", cmd); err != nil {
		t.Fatalf("set-hook: %v", err)
	}

	hooks, err := c.ShowHooks(ctx, "quote")
	if err != nil {
		t.Fatalf("ShowHooks: %v", err)
	}
	got, ok := findHookEntry(t, hooks, "alert-bell", "quote")
	if !ok {
		t.Fatalf("expected alert-bell hook on quote; got %v", hooks)
	}
	if got.Command != cmd {
		t.Fatalf("round-trip mismatch:\n  got: %q\n want: %q", got.Command, cmd)
	}
}

// TestParseHookOutput pins the line-by-line parser against fixtures
// that match what tmux 3.4 actually emits. Keeping this as a pure
// unit test (no live tmux) lets us cover edge cases the integration
// tests can't reliably trigger — multi-binding hooks, embedded quotes,
// trailing carriage returns, and indexed non-hook options that must
// be excluded.
func TestParseHookOutput(t *testing.T) {
	t.Parallel()
	in := strings.Join([]string{
		// A hook with one binding.
		`pane-died[0] display-message "x"`,
		// A hook with multi-binding indices — both must surface as
		// the same Name (the [idx] suffix gets stripped).
		`alert-activity[0] display-message a`,
		`alert-activity[1] display-message b`,
		// An indexed non-hook option that `-H` interleaves into the
		// listing — must be excluded.
		`status-format[0] "#[align=left]something"`,
		// An unset hook (no command body) — must be skipped.
		"client-attached",
		// A trailing CR (a system that preserved \r\n) — must round-trip
		// without leaking the CR into the captured fields.
		`session-created[0] display-message hi` + "\r",
		// A blank line — must be skipped.
		"",
		// A non-indexed plain option — must be skipped (no [idx]).
		"default-shell /bin/bash",
	}, "\n")

	got := parseHookOutput(in, "demo")
	want := []HookEntry{
		{Name: "pane-died", Command: `display-message "x"`, Target: "demo"},
		{Name: "alert-activity", Command: "display-message a", Target: "demo"},
		{Name: "alert-activity", Command: "display-message b", Target: "demo"},
		{Name: "session-created", Command: "display-message hi", Target: "demo"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseHookOutput mismatch\n got: %v\nwant: %v", got, want)
	}
}

// TestParseHookOutput_NonHookPrefixes pins the exclusion list against
// every option family `-H` is known to interleave with hook entries.
// A regression that dropped one of the prefixes would surface as a
// stray "hook" in the output named after a config option.
func TestParseHookOutput_NonHookPrefixes(t *testing.T) {
	t.Parallel()
	for _, family := range []string{
		"status-format[0] some-format",
		"update-environment[0] DISPLAY",
		"command-alias[0] split-pane=split-window",
		"terminal-features[0] xterm-256color",
		"terminal-overrides[0] xterm*:smcup@:rmcup@",
		`user-keys[0] "\033[1;2D"`,
	} {
		t.Run(strings.Split(family, "[")[0], func(t *testing.T) {
			t.Parallel()
			got := parseHookOutput(family, "")
			if len(got) != 0 {
				t.Fatalf("expected zero hooks for non-hook line %q, got %v",
					family, got)
			}
		})
	}
}

// TestShowHooks_NeverNilSlice pins the "always [] never nil" promise
// in every code path: empty server, server with no bindings, and a
// per-session probe with no bindings. The JSON serialiser at the
// boundary stamps a non-nil empty slice as `[]` but stamps nil as
// `null`, so `[]HookEntry{}` is what the wire contract demands.
func TestShowHooks_NeverNilSlice(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "nil_check", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	hooks, err := c.ShowHooks(ctx, "nil_check")
	if err != nil {
		t.Fatalf("ShowHooks: %v", err)
	}
	if hooks == nil {
		t.Fatal("ShowHooks returned a nil slice on a session with no hooks; want []HookEntry{}")
	}
}

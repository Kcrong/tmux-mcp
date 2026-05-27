package tmuxctl

import (
	"context"
	"strings"
	"testing"
	"time"
)

// findBoundCommand walks a `tmux list-keys -T <table>` output looking
// for the entry that binds the supplied key chord. It returns the
// command column verbatim (or the empty string when the binding is
// absent), so tests can assert both presence and the round-trip shape
// of whatever the caller passed into BindKey.
//
// The list-keys output uses `bind-key  [-r]  -T <table>  <key>  <cmd>`
// per row; we already have a parser for that exact shape (see
// parseKeyLine in keys.go), so this helper just leans on ListKeys
// instead of duplicating the regex.
func findBoundCommand(t *testing.T, c *Controller, ctx context.Context, table, key string) string {
	t.Helper()
	keys, err := c.ListKeys(ctx, ListKeysOpts{Table: table})
	if err != nil {
		t.Fatalf("ListKeys(%s): %v", table, err)
	}
	for _, kb := range keys {
		if kb.Key == key {
			return kb.Command
		}
	}
	return ""
}

// TestBindKey_HappyPath_PrefixTable is the load-bearing default-table
// path: passing an empty `table` should land the binding on tmux's
// default key map (prefix on tmux 3.4) and `tmux list-keys -T prefix`
// must echo the same {key, command} back. A regression where the
// boundary forgot to omit `-T` (or fed an empty string for it) would
// trip the assertion because tmux would reject `-T ""` outright.
func TestBindKey_HappyPath_PrefixTable(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "bk", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// "F12" is well outside tmux's default prefix bindings on 3.4 so
	// the assertion is independent of any drift in the built-in map.
	const key = "F12"
	const cmd = "display-message hello"
	if err := c.BindKey(ctx, key, cmd, "", false); err != nil {
		t.Fatalf("BindKey: %v", err)
	}
	got := findBoundCommand(t, c, ctx, "prefix", key)
	if got != cmd {
		t.Fatalf("bind not visible in `list-keys -T prefix`: key=%q got command=%q want %q",
			key, got, cmd)
	}
}

// TestBindKey_CustomTable confirms the `-T TABLE` forwarding: when the
// caller pins `table="copy-mode"`, the binding must land in that table
// (and explicitly NOT in the default prefix table). The sister test
// above pins the empty-table branch; this one pins the populated one.
func TestBindKey_CustomTable(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "bkt", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	const key = "F11"
	const cmd = "display-message custom-table"
	if err := c.BindKey(ctx, key, cmd, "copy-mode", false); err != nil {
		t.Fatalf("BindKey: %v", err)
	}
	if got := findBoundCommand(t, c, ctx, "copy-mode", key); got != cmd {
		t.Fatalf("copy-mode binding missing: key=%q got %q want %q", key, got, cmd)
	}
	// Defence in depth: the same key MUST NOT have leaked into prefix.
	// A regression where the boundary dropped -T would silently install
	// in prefix (the default) — this guard catches that.
	if got := findBoundCommand(t, c, ctx, "prefix", key); got != "" {
		t.Fatalf("F11 also present in prefix table: %q (table forwarding regressed)", got)
	}
}

// TestBindKey_RepeatableFlag exercises the `-r` flag. There is no
// list-keys flag that uniquely surfaces the repeatable bit in a way
// the parser captures (we strip it from the regex), so we instead
// confirm the binding lands at all when -r is set — a regression
// where the boundary swapped `-r` for a positional KEY would either
// hard-error from tmux or land in the wrong slot.
func TestBindKey_RepeatableFlag(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "bkr", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	const key = "F10"
	const cmd = "display-message repeated"
	if err := c.BindKey(ctx, key, cmd, "copy-mode", true); err != nil {
		t.Fatalf("BindKey: %v", err)
	}
	if got := findBoundCommand(t, c, ctx, "copy-mode", key); got != cmd {
		t.Fatalf("repeatable binding missing: key=%q got %q want %q", key, got, cmd)
	}
}

// TestBindKey_CommandWithSpacesSurvivesIntact pins the contract that
// COMMAND travels through tmux as a single argv element. A regression
// where the boundary split the command string on whitespace would
// either error on tmux ("unknown command 'world'") or mangle the
// flags. We prove the round-trip by reading the binding back via
// list-keys and confirming the entire command, including the quoted
// payload, is preserved verbatim.
func TestBindKey_CommandWithSpacesSurvivesIntact(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "bks", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	const key = "F9"
	// Multiple positional words plus a flag — the kind of payload that
	// would obviously fail if anyone split on whitespace.
	const cmd = `display-message -d 0 "hello world from bind"`
	if err := c.BindKey(ctx, key, cmd, "", false); err != nil {
		t.Fatalf("BindKey: %v", err)
	}
	got := findBoundCommand(t, c, ctx, "prefix", key)
	// tmux normalises the rendered command in list-keys (it may
	// re-quote / reformat string args), but the spans we care about
	// must all survive: the verb, the flags, and the embedded payload.
	for _, want := range []string{"display-message", "-d", "0", "hello world from bind"} {
		if !strings.Contains(got, want) {
			t.Fatalf("command not preserved: got %q, missing fragment %q", got, want)
		}
	}
}

// TestBindKey_RejectsEmptyKey is the boundary-layer empty-key guard. A
// JSON-RPC caller that smuggles `key: ""` past the schema check (or
// an in-process Go caller bypassing the MCP layer entirely) must still
// be refused — otherwise tmux would happily bind the empty chord and
// hide the regression behind a binding that can never actually fire.
func TestBindKey_RejectsEmptyKey(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.BindKey(ctx, "", "display-message x", "", false)
	if err == nil {
		t.Fatal("expected error for empty key")
	}
	if !strings.Contains(err.Error(), "key required") {
		t.Fatalf("error %v missing 'key required' phrase", err)
	}
}

// TestBindKey_RejectsEmptyCommand is the symmetrical guard for the
// command argument. Without it, an empty command string would still
// reach tmux and trigger an obscure parse error — this surface
// catches the mistake at the boundary instead.
func TestBindKey_RejectsEmptyCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.BindKey(ctx, "F8", "", "", false)
	if err == nil {
		t.Fatal("expected error for empty command")
	}
	if !strings.Contains(err.Error(), "command required") {
		t.Fatalf("error %v missing 'command required' phrase", err)
	}
}

// TestBindKey_BadCommandPropagatesTmuxError pins the failure-passthrough
// contract: a syntactically broken command string lands at tmux and
// surfaces verbatim through run(). The JSON-RPC layer maps this to
// CodeInternal — we don't fold it into a typed sentinel here because
// bind-key has no equivalent of "session not found" (the failures are
// purely syntactic).
func TestBindKey_BadCommandPropagatesTmuxError(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "bkbad", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// `not-a-real-tmux-command` is rejected by tmux's own parser as an
	// unknown command verb, so any non-nil error proves the boundary
	// surfaced the failure rather than swallowing it.
	if err := c.BindKey(ctx, "F7", "not-a-real-tmux-command", "", false); err == nil {
		t.Fatal("expected error for unknown command verb")
	}
}

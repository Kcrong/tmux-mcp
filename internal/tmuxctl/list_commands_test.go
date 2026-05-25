package tmuxctl

import (
	"context"
	"testing"
	"time"
)

// TestListCommands_DefaultListingIsLarge is the load-bearing "tmux
// ships with a non-trivial command surface" path: every supported
// tmux release advertises dozens of commands (3.0 had ~80, 3.4 has
// ~90), so the unscoped listing must surface at least 30 entries.
// Picking a generous floor keeps the test stable across tmux releases
// while still catching a regression where the boundary forgot a flag
// or the parser silently dropped every line.
func TestListCommands_DefaultListingIsLarge(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	cmds, err := c.ListCommands(ctx, "")
	if err != nil {
		t.Fatalf("ListCommands: %v", err)
	}
	if len(cmds) < 30 {
		t.Fatalf("expected at least 30 commands in the default listing, got %d", len(cmds))
	}
	// Spot-check that every entry parsed cleanly: a stray empty Name
	// would mean the parser silently degraded to a zero-valued
	// CommandInfo, hiding a real tmux drift behind a passing smoke
	// check.
	for i, ci := range cmds {
		if ci.Name == "" {
			t.Fatalf("cmds[%d].Name empty: %#v", i, ci)
		}
	}
}

// TestListCommands_FilterReturnsExactlyOne pins the "filter to a
// known command" path: the supplied name must produce a single-entry
// response whose Name matches the requested verb. Picking
// "list-keys" keeps the test grounded against a command we already
// drive elsewhere on the surface — a regression where the boundary
// forgot to forward the trailing argv would surface as either a
// zero-length response (if tmux ignored the arg and printed nothing
// matching) or a multi-entry one (if it ran the unscoped listing).
func TestListCommands_FilterReturnsExactlyOne(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	cmds, err := c.ListCommands(ctx, "list-keys")
	if err != nil {
		t.Fatalf("ListCommands(list-keys): %v", err)
	}
	if len(cmds) != 1 {
		t.Fatalf("expected exactly one command for list-keys filter, got %d: %#v", len(cmds), cmds)
	}
	if cmds[0].Name != "list-keys" {
		t.Fatalf("cmds[0].Name = %q, want list-keys", cmds[0].Name)
	}
	// list-keys carries the alias "lsk" on every supported tmux
	// release we target — pin it so a regex regression that drops the
	// alias capture trips immediately.
	if cmds[0].Alias != "lsk" {
		t.Fatalf("cmds[0].Alias = %q, want lsk", cmds[0].Alias)
	}
	// list-keys takes flags, so Args must be non-empty. The exact
	// flag set has shifted across tmux releases (`-1aN` vs `-aN`), so
	// we check for non-emptiness rather than pinning the contents.
	if cmds[0].Args == "" {
		t.Fatalf("cmds[0].Args empty for list-keys: %#v", cmds[0])
	}
}

// TestListCommands_FilterUnknownReturnsEmpty pins the documented
// "filter-no-match returns an empty slice" contract. tmux 3.0–3.3
// exits 1 with empty stdout in this case; 3.4+ exits 0 with empty
// stdout. The boundary collapses both into a zero-length slice so
// agents don't have to reason about exit-code drift across tmux
// releases.
func TestListCommands_FilterUnknownReturnsEmpty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	cmds, err := c.ListCommands(ctx, "definitely-not-a-tmux-command-xyzzy")
	if err != nil {
		t.Fatalf("ListCommands(unknown): want nil error, got %v", err)
	}
	if cmds == nil {
		t.Fatal("ListCommands returned nil slice; want zero-length slice for filter-no-match")
	}
	if len(cmds) != 0 {
		t.Fatalf("ListCommands(unknown) returned %d entries, want 0: %#v", len(cmds), cmds)
	}
}

// TestListCommands_AliasParsedWhenPresent guards the alias-capture
// branch on a row that's known to carry one ("kill-window" → "killw"
// on every supported tmux release). Filter to that single command so
// the assertion stays robust against tmux growing or losing other
// commands across releases.
func TestListCommands_AliasParsedWhenPresent(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	cmds, err := c.ListCommands(ctx, "kill-window")
	if err != nil {
		t.Fatalf("ListCommands(kill-window): %v", err)
	}
	if len(cmds) != 1 {
		t.Fatalf("expected exactly one command for kill-window filter, got %d: %#v", len(cmds), cmds)
	}
	if cmds[0].Name != "kill-window" {
		t.Fatalf("cmds[0].Name = %q, want kill-window", cmds[0].Name)
	}
	if cmds[0].Alias != "killw" {
		t.Fatalf("cmds[0].Alias = %q, want killw", cmds[0].Alias)
	}
}

// TestParseCommandLine_NameAliasArgs pins the regex shape against the
// canonical three-column form tmux emits for most commands. A regex
// change that silently drops a column would still pass the "non-empty"
// smoke check on a real tmux but trip this targeted assertion.
func TestParseCommandLine_NameAliasArgs(t *testing.T) {
	t.Parallel()
	got, err := parseCommandLine(`list-keys (lsk) [-1aN] [-P prefix-string] [-T key-table] [key]`)
	if err != nil {
		t.Fatalf("parseCommandLine: %v", err)
	}
	if got.Name != "list-keys" {
		t.Errorf("Name = %q, want list-keys", got.Name)
	}
	if got.Alias != "lsk" {
		t.Errorf("Alias = %q, want lsk", got.Alias)
	}
	if got.Args != `[-1aN] [-P prefix-string] [-T key-table] [key]` {
		t.Errorf("Args = %q, want %q", got.Args, `[-1aN] [-P prefix-string] [-T key-table] [key]`)
	}
}

// TestParseCommandLine_NameOnlyNoAliasNoArgs covers the degenerate
// "kill-server" / "lock-server" / "start-server" rows that tmux
// emits with just a name (often with a trailing space). The boundary
// must surface Alias == "" and Args == "" rather than silently
// degrading.
func TestParseCommandLine_NameOnlyNoAliasNoArgs(t *testing.T) {
	t.Parallel()
	got, err := parseCommandLine(`kill-server`)
	if err != nil {
		t.Fatalf("parseCommandLine: %v", err)
	}
	if got.Name != "kill-server" {
		t.Errorf("Name = %q, want kill-server", got.Name)
	}
	if got.Alias != "" {
		t.Errorf("Alias = %q, want empty", got.Alias)
	}
	if got.Args != "" {
		t.Errorf("Args = %q, want empty", got.Args)
	}
}

// TestParseCommandLine_NoAliasWithArgs covers the "name [args]"
// shape — tmux emits this for commands like clock-mode, choose-tree,
// copy-mode where there is no alias but a non-empty argument
// signature. A regex that accidentally required the alias group would
// silently misparse these rows; pinning a targeted case keeps the
// surface honest.
func TestParseCommandLine_NoAliasWithArgs(t *testing.T) {
	t.Parallel()
	got, err := parseCommandLine(`clock-mode [-t target-pane]`)
	if err != nil {
		t.Fatalf("parseCommandLine: %v", err)
	}
	if got.Name != "clock-mode" {
		t.Errorf("Name = %q, want clock-mode", got.Name)
	}
	if got.Alias != "" {
		t.Errorf("Alias = %q, want empty", got.Alias)
	}
	if got.Args != `[-t target-pane]` {
		t.Errorf("Args = %q, want %q", got.Args, `[-t target-pane]`)
	}
}

// TestParseCommandLine_AliasNoArgs covers the rare "name (alias)" row
// — "lock-server (lock)" is one such case on tmux 3.4. The boundary
// must capture the alias and emit an empty Args field rather than
// folding the alias into Args or rejecting the line.
func TestParseCommandLine_AliasNoArgs(t *testing.T) {
	t.Parallel()
	got, err := parseCommandLine(`lock-server (lock)`)
	if err != nil {
		t.Fatalf("parseCommandLine: %v", err)
	}
	if got.Name != "lock-server" {
		t.Errorf("Name = %q, want lock-server", got.Name)
	}
	if got.Alias != "lock" {
		t.Errorf("Alias = %q, want lock", got.Alias)
	}
	if got.Args != "" {
		t.Errorf("Args = %q, want empty, got %q", got.Args, got.Args)
	}
}

// TestParseCommandLine_RightPaddedColumns confirms the parser tolerates
// the older-tmux shape where the name column is right-padded with
// multiple spaces so the alias / argument columns align across rows.
// The regex should treat the inter-field run as "one or more
// whitespace" so both the modern single-space form and the legacy
// padded form decode the same way.
func TestParseCommandLine_RightPaddedColumns(t *testing.T) {
	t.Parallel()
	got, err := parseCommandLine(`list-keys      (lsk)   [-1aN] [-T key-table]`)
	if err != nil {
		t.Fatalf("parseCommandLine: %v", err)
	}
	if got.Name != "list-keys" {
		t.Errorf("Name = %q, want list-keys", got.Name)
	}
	if got.Alias != "lsk" {
		t.Errorf("Alias = %q, want lsk", got.Alias)
	}
	if got.Args != `[-1aN] [-T key-table]` {
		t.Errorf("Args = %q, want %q", got.Args, `[-1aN] [-T key-table]`)
	}
}

// TestParseCommandLine_RejectsGarbage guards the loud-failure
// contract: a line that doesn't match the
// `<name>[ (<alias>)][ <args>]` shape must error rather than degrade
// to a zero-valued CommandInfo. Silently dropping unrecognised lines
// would hide a tmux drift behind a green test run.
func TestParseCommandLine_RejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := parseCommandLine(""); err == nil {
		t.Fatal("expected error for empty line")
	}
	if _, err := parseCommandLine("(alias-only-no-name)"); err == nil {
		t.Fatal("expected error for line that doesn't start with a command name")
	}
}

// TestIsUnknownCommandMsg pins the substring-based detector that
// powers the "filter-no-match returns empty slice" contract. Older
// tmux phrases the failure several ways; the detector must recognise
// enough variants that the boundary's contract holds across releases.
func TestIsUnknownCommandMsg(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"unknown command: foo":              true,
		"unknown command":                   true,
		"no commands matching: foo":         true,
		"ambiguous command: l":              true,
		"can't find session":                false,
		"":                                  false,
		"some other unrelated tmux failure": false,
	}
	for msg, want := range cases {
		if got := isUnknownCommandMsg(msg); got != want {
			t.Errorf("isUnknownCommandMsg(%q) = %v, want %v", msg, got, want)
		}
	}
}

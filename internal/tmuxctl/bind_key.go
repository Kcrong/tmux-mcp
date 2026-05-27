package tmuxctl

import (
	"context"
	"errors"
	"fmt"
)

// BindKey wraps `tmux bind-key [-T TABLE] [-r] KEY COMMAND` — the write
// counterpart of ListKeys. The boundary registers a single key chord
// against a tmux command string, so an agent that has just discovered a
// gap in the default key map (via list_keys) can install its own
// binding without dropping out to the shell.
//
// Argument shape mirrors the tmux CLI:
//   - key is the chord tmux should match (e.g. "C-Space", "M-x", "Up").
//     tmux accepts any keysym string its parser knows, so the boundary
//     does not pre-validate its content beyond a length / control-byte
//     guard at the JSON-RPC layer (callers send literal keysyms here,
//     not pre-serialised escapes).
//   - command is the tmux command line that should fire when the chord
//     is pressed. tmux's bind-key takes the command as a single argv
//     element; do NOT split on whitespace before passing it in. tmux
//     itself parses the command server-side via `command_parse_string`,
//     and a syntax error there surfaces as a fork/exec failure with the
//     parser message in stderr.
//   - table is the keymap the binding should land in. Empty means "no
//     -T flag" (tmux picks the default keytable, which on tmux 3.4 is
//     "prefix"); a non-empty value is forwarded verbatim as `-T TABLE`.
//     The JSON-RPC layer is responsible for the regex/length guard.
//   - repeatable, when true, adds `-r` so the binding can be repeated
//     while the prefix table stays armed. Off by default; only a small
//     handful of tmux's built-in bindings use it (the resize/select
//     pane chords).
//
// tmux's bind-key emits no stdout on success and reports parse errors
// via stderr. The boundary deliberately does NOT fold any sentinel
// here — bind-key has no equivalent of "session not found" (it does
// not look up live state), so legitimate failures (unknown command
// verb, syntax error inside the command string) are passed through
// verbatim and surface as CodeInternal at the JSON-RPC layer.
//
// The empty-string guards on key/command are kept inside the boundary
// as a defence-in-depth check: even though the JSON-RPC handler also
// rejects empty values, an in-process Go caller that bypasses the MCP
// surface would otherwise let tmux receive `bind-key -T prefix "" ""`
// and silently bind the empty key chord.
func (c *Controller) BindKey(ctx context.Context, key, command, table string, repeatable bool) error {
	if key == "" {
		return errors.New("bind-key: key required")
	}
	if command == "" {
		return errors.New("bind-key: command required")
	}
	args := make([]string, 0, 6)
	args = append(args, "bind-key")
	if repeatable {
		args = append(args, "-r")
	}
	if table != "" {
		args = append(args, "-T", table)
	}
	// KEY and COMMAND are positional. tmux treats COMMAND as a single
	// argv element (its own parser splits it server-side via
	// command_parse_string), so the entire command string travels
	// through here intact — never split it on whitespace.
	args = append(args, key, command)
	if _, err := c.run(ctx, args...); err != nil {
		return fmt.Errorf("bind-key: %w", err)
	}
	return nil
}

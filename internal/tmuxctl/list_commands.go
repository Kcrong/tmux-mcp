package tmuxctl

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// CommandInfo describes a single tmux command surfaced by
// `tmux list-commands`. Each entry corresponds to one row of tmux's
// output; the three fields cover what an agent introspecting the tmux
// command surface actually needs:
//
//   - Name is the canonical command verb (e.g. "list-keys",
//     "send-keys", "kill-server"). Always non-empty for every entry
//     ListCommands returns.
//   - Alias is the short form tmux prints in parentheses next to the
//     name (e.g. "lsk" for "list-keys", "send" for "send-keys"). Empty
//     when the command has no alias — many tmux commands don't, so this
//     field is optional even though it's always present in the JSON
//     output for shape uniformity.
//   - Args is the remaining flag/argument signature tmux printed,
//     verbatim and unparsed. Stays as a single string so a future tmux
//     release can grow new flag forms without the boundary needing a
//     parser update — agents that want to drive a specific flag can
//     match against it with a regex of their own. Empty when the
//     command takes no arguments at all (e.g. "kill-server",
//     "lock-server", "start-server").
type CommandInfo struct {
	// Name is the canonical command verb.
	Name string
	// Alias is the short form, or empty when the command has no alias.
	Alias string
	// Args is the rest of the line — flags, positionals, and the like —
	// returned verbatim so agents see exactly what tmux printed.
	Args string
}

// listCommandsLineRE recognises a single line of `tmux list-commands`
// output. tmux 3.0+ emits the shape:
//
//	<name> [(<alias>)] [<args>]
//
// where:
//   - <name> is the canonical command verb (alnum + dashes only).
//   - <alias>, when present, is the short form in parentheses.
//   - <args> is the rest of the line — flags, positionals, examples —
//     returned verbatim so agents see what tmux printed.
//
// Older tmux releases right-pad <name> with spaces so the alias /
// argument columns align across rows; tmux 3.4 onward has dropped the
// padding and emits a single space between <name> and the next field.
// The regex captures both shapes by treating the inter-field run as
// "one or more whitespace" (`\s+`) — defensive parsing that works on
// every supported tmux build without a version branch.
//
// We deliberately do NOT use `tmux list-commands -F '...'`: the `-F`
// flag was added to `list-commands` in tmux 3.2, and tmux-mcp's
// minimum supported version is 3.0. Parsing the default output keeps
// the boundary working on every supported release.
var listCommandsLineRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*)(?:\s+\(([A-Za-z][A-Za-z0-9-]*)\))?(?:\s+(.*))?$`)

// ListCommands returns every tmux command tmux's `list-commands`
// surface currently advertises. When name is empty the unscoped
// listing is returned (every command on this tmux build); when name
// is non-empty tmux is asked to restrict the output to that one
// command's signature (`tmux list-commands NAME`).
//
// Empty stdout is returned as `[]CommandInfo{}` (a zero-length slice,
// never nil) so the JSON-RPC layer can serialise `{"commands": []}`
// cleanly. This is the load-bearing case for the "filter to a name
// tmux does not know about" path: tmux 3.0–3.3 exits 1 with no
// output, tmux 3.4+ exits 0 with no output. Both surface as an empty
// slice — the boundary doesn't ask agents to reason about exit-code
// drift across tmux releases.
//
// Other failures (the tmux server failing to spawn, a parse miss on a
// malformed line) pass through unchanged so the dispatcher surfaces
// them via CodeInternal. A parse miss is loud on purpose: silently
// dropping a line we don't recognise would hide a tmux drift the test
// suite needs to flag.
func (c *Controller) ListCommands(ctx context.Context, name string) ([]CommandInfo, error) {
	args := []string{"list-commands"}
	if name != "" {
		// tmux interprets a trailing positional as "show only this
		// command". Forwarded verbatim — the JSON-RPC layer is
		// responsible for the input shape (regex / length policy)
		// before this function ever runs.
		args = append(args, name)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		// Older tmux (3.0–3.3) exits 1 with empty stdout when the
		// `list-commands NAME` filter does not match a known command;
		// 3.4+ exits 0. We collapse both into "empty result" so callers
		// see a uniform empty slice regardless of which build is on
		// $PATH. Anything else (tmux server failures, malformed
		// invocations) is a real error and surfaces upstream.
		if name != "" && isUnknownCommandMsg(err.Error()) {
			return []CommandInfo{}, nil
		}
		return nil, err
	}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		// tmux 3.4+ filter-no-match path: clean exit, empty stdout.
		// Surface the same empty slice the older-tmux branch above
		// produces so the boundary is version-uniform.
		return []CommandInfo{}, nil
	}
	lines := strings.Split(out, "\n")
	cmds := make([]CommandInfo, 0, len(lines))
	for i, line := range lines {
		// Trim trailing whitespace tmux occasionally emits on
		// no-argument rows ("kill-server ", "lock-server (lock) ").
		// Leading whitespace would never occur in a valid row, but
		// trimming it costs nothing and keeps the parser tolerant of
		// future tmux output tweaks.
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		ci, perr := parseCommandLine(trimmed)
		if perr != nil {
			return nil, fmt.Errorf("list-commands: line %d: %w", i+1, perr)
		}
		cmds = append(cmds, ci)
	}
	return cmds, nil
}

// parseCommandLine splits one line of `tmux list-commands` output into
// a CommandInfo. The shape is `<name>[ (<alias>)][ <args>]`; the regex
// captures all three components in one pass and returns an error when
// the line doesn't match — a parse miss is almost certainly a tmux
// drift the test suite needs to flag, not a data-loss surface to
// swallow.
func parseCommandLine(line string) (CommandInfo, error) {
	m := listCommandsLineRE.FindStringSubmatch(line)
	if m == nil {
		return CommandInfo{}, fmt.Errorf("unrecognised list-commands line: %q", line)
	}
	return CommandInfo{
		Name:  m[1],
		Alias: m[2],
		// m[3] may be empty when the command takes no arguments
		// ("kill-server", "lock-server", "start-server"); strip any
		// trailing whitespace tmux added so the field round-trips
		// cleanly through JSON.
		Args: strings.TrimRight(m[3], " "),
	}, nil
}

// isUnknownCommandMsg reports whether stderr text from `tmux
// list-commands NAME` indicates the supplied name does not match a
// known command on this tmux build. Older tmux phrases this several
// ways ("unknown command: <name>", "no commands matching <name>"); we
// recognise enough variants that the boundary's "filter-no-match
// returns empty slice" contract holds across the supported version
// range.
//
// The check is deliberately substring-based: tmux's exact phrasing
// has shifted across releases and the ListCommands contract treats
// "no match" as a non-error condition regardless of how the build on
// $PATH happens to spell it.
func isUnknownCommandMsg(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "unknown command") ||
		strings.Contains(m, "no commands matching") ||
		strings.Contains(m, "ambiguous command")
}

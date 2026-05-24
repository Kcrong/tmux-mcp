package tmuxctl

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// KeyBinding describes a single tmux key binding as observed by
// `tmux list-keys`. The fields cover the three columns an agent
// introspecting the controller's key map actually cares about: which
// table the binding belongs to, the key chord that triggers it, and the
// command (or annotated note) that fires.
//
// The shape stays uniform across the two output modes tmux supports:
//   - Default ("bind-key" form): Command is the tmux command line that
//     fires when the key chord is pressed.
//   - Notes-only (`-N`): Command carries the human-readable note text
//     attached to the binding via `bind-key -N "..."`. tmux deliberately
//     suppresses bindings without a note in this mode, so the response
//     shrinks to the documented subset.
//
// Table is the binding's keymap (e.g. "prefix", "root", "copy-mode",
// "copy-mode-vi"). When the caller supplied a `-T TABLE` filter, every
// returned entry has Table == TABLE; in the unscoped variant Table is
// whatever tmux printed in the `bind-key -T <table>` column. In the
// notes-only mode tmux drops the table column entirely from its output,
// so Table is propagated from the caller's `-T` arg when present and
// left empty otherwise — see ListKeys for the exact contract.
type KeyBinding struct {
	// Table is the keymap the binding lives in. Empty only in the
	// notes-only mode without a `-T` filter (where tmux does not print
	// the column and the boundary has no other ground truth).
	Table string
	// Key is the key chord (e.g. "C-a", "M-{", "Space", "Enter"). When
	// the controller asked for `-P prefix`, tmux renders the chord with
	// that prefix prepended in notes-only mode; the boundary returns the
	// rendered form verbatim so the caller sees what tmux produced.
	Key string
	// Command is the action the binding triggers. In the default
	// rendering this is a tmux command line (potentially containing
	// embedded `{...}` blocks); in notes-only mode it is the binding's
	// `-N` note text instead.
	Command string
}

// ListKeysOpts narrows the `tmux list-keys` invocation. The zero value
// is "every binding in every table, in the default (bind-key) form" —
// equivalent to running `tmux list-keys` with no flags. Callers opt in
// to filters one knob at a time so the boundary stays additive.
type ListKeysOpts struct {
	// Table, when non-empty, scopes the listing to a single keymap by
	// passing `-T TABLE`. Common values are "prefix" (the default
	// post-C-b table), "root" (no-prefix bindings), and "copy-mode" /
	// "copy-mode-vi" (search/selection bindings while in copy mode).
	// Empty means "every table" (no `-T` flag is added).
	Table string
	// NotesOnly switches the listing into the `-N` mode tmux uses for
	// the user-visible help screens: only bindings annotated with a
	// `bind-key -N "..."` note are printed, and the third column is the
	// note text rather than the command. Defaults to false (the full
	// listing including bindings without notes).
	NotesOnly bool
	// Prefix, when non-empty, is forwarded as `-P PREFIX`; tmux uses it
	// to prefix the rendered key chord in the output (only in the
	// notes-only mode — tmux's own `-P` semantics). The boundary keeps
	// the rendered prefix in the returned Key so the caller sees what
	// tmux produced verbatim. Empty means "no `-P` flag" (no prefix
	// prepended).
	Prefix string
}

// bindKeyLineRE matches a single line of the default (non-`-N`) tmux
// list-keys output. The shape is:
//
//	bind-key  [-r] [-n]  -T <table>  <key>          <command...>
//
// where:
//   - `-r` (repeat) and `-n` (no-prefix shorthand) are optional flags
//     tmux may emit for some entries.
//   - <table> is whitespace-free.
//   - <key> is either a single whitespace-free token (e.g. `C-a`,
//     `Enter`, `M-Up`) or a double-quoted token (e.g. `"M-{"` for keys
//     that contain a literal `{`/`}` that would otherwise be ambiguous
//     with tmux's command-block syntax).
//   - <command> is the rest of the line; it may contain anything,
//     including embedded `{...}` command blocks with their own internal
//     whitespace, so the regex captures it greedily to end-of-line.
//
// The leading `^` and trailing `$` are anchored so a line we don't
// recognise (a stray banner, an empty line, the "no current target" CLI
// echo on some builds) does not silently slip through as a malformed
// binding.
//
// We deliberately do NOT use `tmux list-keys -F '#{key_table}|...'`:
// the `-F` flag was added to `list-keys` in tmux 3.5, and tmux-mcp's
// minimum supported version is 3.0. Parsing the default output keeps
// the boundary working on every supported release.
var bindKeyLineRE = regexp.MustCompile(`^bind-key\s+(?:-r\s+)?(?:-n\s+)?-T\s+(\S+)\s+(\S+|"[^"]*")\s+(.*)$`)

// ListKeys enumerates the tmux key bindings on this controller's tmux
// server. The opts struct narrows the listing through the three knobs
// the boundary currently exposes (table filter, notes-only mode,
// rendered key prefix); see ListKeysOpts for the per-field contract.
//
// Empty stdout (no matching bindings — common for `key_table` = a
// custom user table that has not been populated, or `notes_only` over
// a table where every binding lacks a note) returns []KeyBinding{} (a
// zero-length slice, never nil) so callers can iterate the response
// without a separate "is this an error" branch and the JSON-RPC layer
// can serialise `{"keys": []}` cleanly.
//
// Other tmux failures pass through unchanged so the dispatcher surfaces
// them via CodeInternal. The most common failure on the boundary is
// "table TABLE doesn't exist" when `Table` names a keymap tmux does not
// know about — that surface is preserved verbatim; the JSON-RPC layer
// maps it to a generic internal error rather than a typed sentinel
// because there is no equivalent of "session not found" for a missing
// key table (it is a typo, not a runtime mutation of state).
func (c *Controller) ListKeys(ctx context.Context, opts ListKeysOpts) ([]KeyBinding, error) {
	args := []string{"list-keys"}
	if opts.NotesOnly {
		// `-N` switches the output to "bindings annotated with a note"
		// in a two-column "<key>  <note>" form. tmux emits just the
		// keys that have a `-N` note; everything else is suppressed.
		args = append(args, "-N")
	}
	if opts.Table != "" {
		args = append(args, "-T", opts.Table)
	}
	if opts.Prefix != "" {
		// `-P` is a render-time decoration: tmux prepends the prefix to
		// every key chord in the output when in notes-only mode. We
		// pass it through verbatim and surface the rendered Key so the
		// caller sees what tmux produced.
		args = append(args, "-P", opts.Prefix)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		// No matching bindings (common for narrow filters). Return a
		// zero-length slice — never nil — so the JSON-RPC layer
		// serialises `{"keys": []}` instead of `{"keys": null}`.
		return []KeyBinding{}, nil
	}
	lines := strings.Split(out, "\n")
	// keyColumn is the parser's hint for the notes-only mode: tmux pads
	// the key column to a fixed width derived from the longest key
	// chord in the output (sprintf `%-<width>s`), so when one entry's
	// chord is *exactly* that width tmux emits a single space before
	// the note. We pre-scan the output to discover the column width
	// from any line that *does* have a 2+ space gap, then apply that
	// width uniformly to every line. -1 means "no hint, fall back to
	// the line-local 2+ space split" — which is the right behaviour in
	// default mode (where the regex carries the column boundaries) and
	// in notes-only listings where every entry happens to fit comfortably.
	keyColumn := -1
	if opts.NotesOnly {
		keyColumn = detectNotesKeyColumn(lines)
	}
	keys := make([]KeyBinding, 0, len(lines))
	for i, line := range lines {
		// Skip blank lines defensively — tmux occasionally emits a
		// trailing empty separator on some builds when both `-N` and a
		// table filter are combined.
		if strings.TrimSpace(line) == "" {
			continue
		}
		kb, perr := parseKeyLine(line, opts, keyColumn)
		if perr != nil {
			return nil, fmt.Errorf("list-keys: line %d: %w", i+1, perr)
		}
		keys = append(keys, kb)
	}
	return keys, nil
}

// detectNotesKeyColumn scans the notes-only output and returns the
// width tmux is padding the key column to, or -1 when no line in the
// output has the 2+ space gap that would reveal the boundary.
//
// tmux prints each `-N` line as `<key:%-<W>s> <note>` where W is the
// max key-chord length across all entries. When one entry's chord is
// exactly W characters long the printed line shows a single-space gap
// — the same shape a chord-with-internal-space would produce — so a
// purely line-local "first 2+ spaces" split misclassifies these
// rows. By taking the *minimum* gap-start column observed across the
// listing we recover W: every shorter chord produces a gap that begins
// at column W (the padding fills the rest), and the parser can then
// split every line at column W deterministically.
//
// Returning -1 (no hint) is safe: parseKeyLine falls back to the
// line-local 2+ space split, which is correct whenever every chord in
// the listing is short enough to leave at least 2 trailing spaces of
// padding.
func detectNotesKeyColumn(lines []string) int {
	col := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		idx := indexRunOfSpaces(line, 2)
		if idx < 0 {
			continue
		}
		if col == -1 || idx < col {
			col = idx
		}
	}
	return col
}

// parseKeyLine splits one line of `tmux list-keys` output into a
// KeyBinding. The output mode (default vs notes-only) is decided by
// opts.NotesOnly because the two shapes are different and trying to
// auto-detect would hide a bug where the boundary forgot to forward a
// flag.
//
// In default mode the regex bindKeyLineRE matches the
// `bind-key [-r] [-n] -T <table> <key>  <command...>` form; the three
// captures map directly to KeyBinding's fields.
//
// In notes-only mode the line is the two-column "<key>  <note>" form
// padded by tmux to a fixed-width key column. keyColumn is the
// pre-scanned padding width (see detectNotesKeyColumn) — when ≥ 0 we
// split every line at that column so the entry whose chord is *exactly*
// the column width (and therefore has only one space of padding before
// the note) parses correctly. When keyColumn is < 0 we fall back to
// the first-run-of-2+-spaces heuristic, which is correct whenever every
// chord is short enough to leave breathing room.
//
// Table is propagated from opts.Table when set so the response shape
// stays uniform; otherwise it is left empty (tmux's notes-only output
// does not carry the table column).
//
// Lines we don't recognise return an error rather than silently
// degrading to a zero-valued KeyBinding — a parse miss is almost
// certainly a tmux version drift the test suite needs to flag, not a
// data-loss surface to swallow.
func parseKeyLine(line string, opts ListKeysOpts, keyColumn int) (KeyBinding, error) {
	if opts.NotesOnly {
		// When detectNotesKeyColumn found a stable column boundary,
		// honour it: it's the only way to correctly parse the entries
		// whose chord is *exactly* the column width (and therefore have
		// only a single trailing space of padding).
		var key, note string
		if keyColumn >= 0 && keyColumn < len(line) {
			key = strings.TrimRight(line[:keyColumn], " ")
			note = strings.TrimLeft(line[keyColumn:], " ")
		} else {
			// Fall back to the line-local heuristic. This branch fires
			// for short listings where every chord left at least two
			// trailing spaces of padding, or for the degenerate case
			// of a single-line output.
			idx := indexRunOfSpaces(line, 2)
			if idx < 0 {
				return KeyBinding{}, fmt.Errorf("notes-only line missing column gap: %q", line)
			}
			key = strings.TrimRight(line[:idx], " ")
			note = strings.TrimLeft(line[idx:], " ")
		}
		if key == "" || note == "" {
			return KeyBinding{}, fmt.Errorf("notes-only line missing key or note: %q", line)
		}
		return KeyBinding{
			Table:   opts.Table,
			Key:     key,
			Command: note,
		}, nil
	}
	m := bindKeyLineRE.FindStringSubmatch(line)
	if m == nil {
		return KeyBinding{}, fmt.Errorf("unrecognised bind-key line: %q", line)
	}
	// Strip the surrounding double-quotes tmux adds to keys whose name
	// would otherwise be ambiguous with its command-block syntax (e.g.
	// `"M-{"`). The unquoted form is what callers feed back into
	// `bind-key` / `send-keys`, so returning it bare keeps the
	// round-trip clean.
	key := m[2]
	if len(key) >= 2 && key[0] == '"' && key[len(key)-1] == '"' {
		key = key[1 : len(key)-1]
	}
	return KeyBinding{
		Table:   m[1],
		Key:     key,
		Command: strings.TrimRight(m[3], " "),
	}, nil
}

// indexRunOfSpaces returns the index of the first run of at least n
// consecutive ASCII spaces in s, or -1 when no such run exists. Used
// by the notes-only parser to find the column gap between key chord
// and note in tmux's `-N` output. A negative or zero n is rejected as
// a programmer error: the caller-facing contract is "find a real run",
// and a zero-length run is meaningless.
func indexRunOfSpaces(s string, n int) int {
	if n <= 0 {
		return -1
	}
	run := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			run++
			if run == n {
				return i - n + 1
			}
			continue
		}
		run = 0
	}
	return -1
}

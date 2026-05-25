package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// OptionEntry is the per-row shape returned by [Controller.ShowWindowOptions].
// It is the structured counterpart to the flat `key value` pair tmux
// emits for each option: Name carries the option key (e.g.
// `synchronize-panes`, `mode-keys`, or an array-style key like
// `command-alias[0]`), Value carries the verbatim text tmux printed
// after the first space — including any surrounding quoting tmux
// chose to emit for strings with embedded specials.
//
// Keeping this as a typed struct (rather than reusing the plain
// map[string]string [Controller.ShowOptions] returns) lets the JSON-RPC
// surface emit a deterministic ordered array — `tmux show-window-options`
// already prints alphabetically, and a slice preserves that order on the
// wire so callers iterating the result see the same stable layout tmux
// itself uses. A future addition (say, a "scope" tag distinguishing
// `-g` defaults from per-window overrides) can land as a new field on
// this struct without breaking the existing field set.
type OptionEntry struct {
	// Name is the option key — a single token from the start of the line.
	Name string
	// Value is the remainder of the line tmux emitted, returned verbatim
	// (including any quoting). Empty when tmux printed no value.
	Value string
}

// ShowWindowOptions wraps `tmux show-window-options [-gv] [-t TARGET] [OPTION]`
// and returns the resolved window-options table at the requested target.
// It is the read-side sibling of `set-window-option`: where set_window_option
// mutates a single per-window flag (synchronize-panes, automatic-rename,
// mode-keys, …), ShowWindowOptions reports the current values so an
// agent can introspect what the live window is configured to do.
//
// Argument semantics:
//
//   - target: when non-empty, passed as `-t TARGET` so tmux scopes the
//     query to a specific window. Either `<session>` or
//     `<session>:<window>` is accepted; tmux resolves both forms. Empty
//     means "no -t flag", in which case tmux uses its current target
//     (typically the most recently active window).
//   - name: when non-empty, appended as the trailing positional so tmux
//     returns just that single option. The result slice will hold at
//     most one entry — empty when the option is unset on the target
//     (tmux prints nothing in that case).
//   - global: when true, prepends `-g` so tmux reports the global
//     window-options table (the `-g` defaults) instead of the per-window
//     overrides. Mirrors the `global` knob on [Controller.ShowOptions].
//
// Output shape:
//
// `tmux show-window-options` prints one `key value` pair per line; the
// key is always a single token, the value is the remainder (which may
// contain spaces, e.g. `mode-style fg=black,bg=yellow`). The reused
// [parseShowOptions] helper handles the line-parsing — we just walk the
// resulting map into a deterministic slice ordered by key so the JSON
// surface mirrors what `tmux show-window-options` itself prints.
//
// Error mapping:
//
// A missing session/window is normalised to a wrapped
// errs.ErrSessionNotFound so the JSON-RPC dispatcher can surface a
// stable CodeSessionNotFound (-32000) regardless of which exact phrase
// tmux emitted. The run() helper already catches "can't find session" /
// "no current session" / "session not found" / "no such session"; we
// additionally catch "no such window" here because that is the phrase
// tmux 3.4 prints for `show-window-options -t <missing>`. Returning a
// typed sentinel keeps callers (tests, telemetry) from substring-matching
// the version-dependent stderr.
//
// An empty result (no matching option, or no overrides set on the
// target without -g) returns an empty slice, not an error: it is a
// well-formed query that simply has nothing to report.
func (c *Controller) ShowWindowOptions(ctx context.Context, target, name string, global bool) ([]OptionEntry, error) {
	args := buildShowWindowOptionsArgs(target, name, global)
	out, err := c.run(ctx, args...)
	if err != nil {
		// run() already wraps "can't find session" / "no current session"
		// / "session not found" / "no such session" via isSessionMissingMsg.
		// tmux 3.4's `show-window-options -t <missing>` emits "no such
		// window: <target>", which is not on that list — surface it as the
		// same sentinel here so the dispatcher maps it to
		// CodeSessionNotFound. errors.Is() on the original error keeps the
		// wrapping idempotent for the cases run() already handled.
		if errors.Is(err, errs.ErrSessionNotFound) {
			return nil, err
		}
		if isWindowMissingMsg(err.Error()) {
			return nil, fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return nil, err
	}
	parsed := parseShowOptions(out)
	if len(parsed) == 0 {
		return []OptionEntry{}, nil
	}
	// `tmux show-window-options` prints alphabetically; preserve that
	// ordering on the wire so callers iterating the slice see the same
	// stable layout tmux itself uses. The map walk in parseShowOptions
	// loses the input ordering, so we reconstruct it from the original
	// stdout — line-by-line walk, dedup against the parsed map.
	seen := make(map[string]bool, len(parsed))
	entries := make([]OptionEntry, 0, len(parsed))
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		key := line
		if i := strings.Index(line, " "); i >= 0 {
			key = line[:i]
		}
		if key == "" || seen[key] {
			continue
		}
		val, ok := parsed[key]
		if !ok {
			continue
		}
		seen[key] = true
		entries = append(entries, OptionEntry{Name: key, Value: val})
	}
	return entries, nil
}

// buildShowWindowOptionsArgs assembles the argv passed to `tmux
// show-window-options`. Split out from [Controller.ShowWindowOptions] so
// the assembly logic can be unit-tested without spinning up a live tmux
// server. The resulting slice always starts with the `show-window-options`
// verb and appends `-g`, `-t TARGET`, and the trailing OPTION positional
// only when the corresponding argument is non-empty / true.
func buildShowWindowOptionsArgs(target, name string, global bool) []string {
	args := []string{"show-window-options"}
	if global {
		args = append(args, "-g")
	}
	if target != "" {
		args = append(args, "-t", target)
	}
	if name != "" {
		args = append(args, name)
	}
	return args
}

// isWindowMissingMsg reports whether stderr text from tmux indicates the
// targeted window does not exist. tmux 3.4 phrases this as "no such
// window: <target>" for show-window-options against an unknown
// session/window; the broader missing-session phrasing is already
// covered by [isSessionMissingMsg] inside run(). Detect both phrasings
// here so [Controller.ShowWindowOptions] can rely on
// errs.ErrSessionNotFound regardless of the phrase tmux chose.
func isWindowMissingMsg(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "no such window")
}

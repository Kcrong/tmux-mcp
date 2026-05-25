package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// FindWindowOpts mirrors the flag surface of `tmux find-window`. Each
// "*Only" toggle restricts where the search runs; setting more than one
// is treated as a union (matches in any selected scope are returned),
// matching the way tmux's own `-NCT` flags compose. When all three
// toggles are false the default behaviour is "search across name,
// title, and visible content" — same as tmux find-window's default
// `-CNT`.
//
// Regex flips the comparison from fnmatch-style globbing (default) to
// regular expressions (`-r` in tmux find-window). Target restricts the
// search to a single session via `list-windows -t <session>`; an empty
// Target enumerates every window on the server (`-a`).
type FindWindowOpts struct {
	// NameOnly restricts matching to the window name (`-N`).
	NameOnly bool
	// TitleOnly restricts matching to the window's pane title (`-T`).
	TitleOnly bool
	// ContentOnly restricts matching to visible pane content (`-C`).
	ContentOnly bool
	// Regex switches the match expression from fnmatch globs to a
	// regular expression (`-r`).
	Regex bool
	// Target, when non-empty, scopes the search to a single tmux session
	// — same as `list-windows -t <session>`. Empty means "every window
	// on the server" (`-a`).
	Target string
}

// WindowMatch is one row of the [Controller.FindWindow] result. The
// session/index/name triple is the minimum an agent needs to build a
// follow-up `<session>:<window>` target string for capture, send_keys,
// or window_kill — so we keep the surface tight rather than echoing
// every find-window field tmux could produce.
type WindowMatch struct {
	// Session is the tmux session the matching window lives in.
	Session string
	// WindowIndex is the numeric index of the window within its session
	// (`#{window_index}`).
	WindowIndex int
	// WindowName is the human-readable label tmux assigned to the window
	// (`#{window_name}`).
	WindowName string
}

// findWindowFormat is the `-F` template list-windows substitutes for
// each row that survives the filter. Kept in lockstep with
// parseFindWindowLine — both files have to be edited in one CL if
// either side changes.
const findWindowFormat = "#{session_name}|#{window_index}|#{window_name}"

// FindWindow enumerates every window whose name, pane title, or
// visible content matches `match`, returning a flat slice of
// [WindowMatch] rows. Functionally this is `tmux find-window` for a
// headless server: tmux's own find-window requires an attached client
// (and emits no machine-readable output even with one), so we use
// `tmux list-windows -F <format> -f <filter>` with format-string
// matchers to recreate the same semantics in a way that works on a
// detached server.
//
// `match` is required; an empty value is rejected up front so a stray
// "match every window" call cannot mask a typo. Opts.NameOnly /
// TitleOnly / ContentOnly compose as a union — set none of them to get
// the default `-CNT` behaviour (match across all three scopes), the
// same default tmux find-window applies. Opts.Regex toggles between
// fnmatch (default) and regular-expression matching, mirroring
// find-window's `-r`. Opts.Target scopes the search to a single
// session (`list-windows -t`), matching the way `find-window -t`
// targets a pane in tmux's own surface.
//
// On no matches the return is an empty slice (NOT nil) plus a nil
// error, so callers can treat "no rows" as a valid query result rather
// than an error. A missing target session surfaces as a wrapped
// errs.ErrSessionNotFound (via run()'s built-in detection) so the
// JSON-RPC layer can map it to CodeSessionNotFound the same way every
// other session-bearing tool does.
func (c *Controller) FindWindow(ctx context.Context, match string, opts FindWindowOpts) ([]WindowMatch, error) {
	if match == "" {
		return nil, errors.New("match required")
	}
	args := []string{"list-windows", "-F", findWindowFormat}
	if opts.Target != "" {
		args = append(args, "-t", opts.Target)
	} else {
		// -a means "every window on the server" — the headless equivalent
		// of find-window's behaviour when the operator did not pin a
		// target pane.
		args = append(args, "-a")
	}
	args = append(args, "-f", buildFindWindowFilter(match, opts))
	out, err := c.run(ctx, args...)
	if err != nil {
		// list-windows -t <missing> emits "can't find session" which
		// run() already translates to errs.ErrSessionNotFound. Some tmux
		// builds phrase the same condition as "can't find window" or
		// "no server running" — fold those into the typed sentinel too
		// so callers can rely on a single error type for "session does
		// not exist on this controller".
		if opts.Target != "" && !errors.Is(err, errs.ErrSessionNotFound) {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "can't find window") ||
				strings.Contains(msg, "no server running") {
				return nil, fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
			}
		}
		return nil, err
	}
	out = strings.TrimSpace(out)
	// An empty filter result is a perfectly valid "no matches" outcome,
	// not an error. Return an explicit non-nil empty slice so the
	// JSON-RPC layer serialises `[]` rather than `null` — agents that
	// branch on `len(arr) == 0` should not have to also handle the null
	// shape.
	matches := make([]WindowMatch, 0)
	if out == "" {
		return matches, nil
	}
	for i, line := range strings.Split(out, "\n") {
		m, perr := parseFindWindowLine(line)
		if perr != nil {
			return nil, fmt.Errorf("find-window: line %d: %w", i+1, perr)
		}
		matches = append(matches, m)
	}
	return matches, nil
}

// buildFindWindowFilter assembles the tmux format-filter expression
// that powers the search. The scope flags compose as a union — when
// more than one is set we OR them together with `#{||:a,b}`; when none
// is set the default is `name OR title OR content`, matching the
// tmux find-window default of `-CNT`.
//
// fnmatch (the default) wraps the user's match string in `*…*` so a
// substring like "match" finds windows named "*match_me*". Regex mode
// hands the pattern through verbatim — tmux's `m/r` operator already
// treats it as a partial match so callers only need to anchor when
// they specifically want a full-line match.
func buildFindWindowFilter(match string, opts FindWindowOpts) string {
	parts := make([]string, 0, 3)
	if opts.NameOnly {
		parts = append(parts, nameMatchExpr(match, opts.Regex))
	}
	if opts.TitleOnly {
		parts = append(parts, titleMatchExpr(match, opts.Regex))
	}
	if opts.ContentOnly {
		parts = append(parts, contentMatchExpr(match, opts.Regex))
	}
	if len(parts) == 0 {
		// Default scope — name OR title OR content, matching tmux
		// find-window's default `-CNT` behaviour.
		parts = []string{
			nameMatchExpr(match, opts.Regex),
			titleMatchExpr(match, opts.Regex),
			contentMatchExpr(match, opts.Regex),
		}
	}
	return joinFilterOR(parts)
}

// nameMatchExpr builds the tmux `#{m...}` predicate that tests the
// window name. Substring matching is the default (we wrap in `*…*` so
// "build" matches "build" anywhere in the name); regex flips to
// `#{m/r:...}` and uses the caller's pattern verbatim.
func nameMatchExpr(match string, regex bool) string {
	if regex {
		return "#{m/r:" + match + ",#{window_name}}"
	}
	return "#{m:*" + match + "*,#{window_name}}"
}

// titleMatchExpr builds the predicate that tests the window's pane
// title (the active pane's `#{pane_title}`, which is also what tmux
// find-window's `-T` matches against).
func titleMatchExpr(match string, regex bool) string {
	if regex {
		return "#{m/r:" + match + ",#{pane_title}}"
	}
	return "#{m:*" + match + "*,#{pane_title}}"
}

// contentMatchExpr builds the predicate that tests visible pane
// content. tmux's `#{C:...}` expression searches the visible buffer
// directly; the regex variant `#{C/r:...}` applies a regex over the
// same scope.
func contentMatchExpr(match string, regex bool) string {
	if regex {
		return "#{C/r:" + match + "}"
	}
	return "#{C:" + match + "}"
}

// joinFilterOR composes a slice of tmux predicates into a single
// expression by chaining `#{||:a,b}` operators. tmux's `||` is binary,
// so a 3-way union becomes `#{||:a,#{||:b,c}}`. Returns the lone
// element verbatim when there is only one — wrapping a single
// predicate in `||` is harmless but noisier than necessary.
func joinFilterOR(parts []string) string {
	switch len(parts) {
	case 0:
		// Should be unreachable — buildFindWindowFilter always populates
		// at least one entry — but defending against an empty slice
		// keeps the helper safe to reuse.
		return ""
	case 1:
		return parts[0]
	}
	expr := parts[len(parts)-1]
	for i := len(parts) - 2; i >= 0; i-- {
		expr = "#{||:" + parts[i] + "," + expr + "}"
	}
	return expr
}

// parseFindWindowLine splits one '|'-delimited row produced by
// findWindowFormat into a [WindowMatch]. The format is fixed at the
// call site (above), so any drift in field count is a bug — reject it
// loudly rather than guess. The boundary regex/length policy on
// session and window names already excludes '|', so the literal
// separator round-trips safely.
func parseFindWindowLine(line string) (WindowMatch, error) {
	const wantFields = 3
	fields := strings.Split(line, "|")
	if len(fields) != wantFields {
		return WindowMatch{}, fmt.Errorf("expected %d '|'-separated fields, got %d in %q",
			wantFields, len(fields), line)
	}
	idx, err := strconv.Atoi(fields[1])
	if err != nil {
		return WindowMatch{}, fmt.Errorf("window_index %q: %w", fields[1], err)
	}
	return WindowMatch{
		Session:     fields[0],
		WindowIndex: idx,
		WindowName:  fields[2],
	}, nil
}

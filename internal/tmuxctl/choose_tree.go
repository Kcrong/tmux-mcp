package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// ChooseTreeRow describes a single (session, window) pair as observed
// by the snapshot form of `tmux choose-tree`. It is the structured
// counterpart of one line in tmux's interactive picker — the LLM-facing
// surface uses this to "see the whole topology" of the server in a
// single tool call without having to iterate list_sessions ×
// list_windows × list_panes.
//
// Fields cover the values an agent typically switches on: the session
// name and window index for routing follow-up calls (capture,
// send_keys, window_select, ...), the window name for human-readable
// labels, the pane count for layout-aware logic, and the active flag
// so the agent knows which window is currently focused.
type ChooseTreeRow struct {
	// Session is the tmux session name this row belongs to. Equal to
	// the value an agent would pass on follow-up calls (capture,
	// send_keys, ...).
	Session string
	// WindowIndex is the numeric window index (0-based) within the
	// session. Combined with Session it forms the canonical
	// "session:index" target string.
	WindowIndex int
	// WindowName is the human-readable label tmux assigned to the
	// window. May be the caller-supplied -n value or whatever tmux
	// auto-assigned from the command's basename.
	WindowName string
	// PaneCount is the number of panes currently in the window. Useful
	// for layout-aware agents that need to know whether a window is
	// split before issuing follow-up pane-targeted calls.
	PaneCount int
	// Active reports whether this window is the currently focused one
	// of its session — i.e. tmux's `#{window_active}` flag.
	Active bool
}

// chooseTreeFormat matches the parsing in parseChooseTreeLine — keep
// them in sync. tmux substitutes each #{...} variable and joins them
// with the literal '|' between them. '|' is safe here because none of
// these variables ever contains it (the boundary validator already
// forbids it in user-supplied session and window names, and the
// numeric/boolean fields never produce one).
const chooseTreeFormat = "#{session_name}|#{window_index}|#{window_name}|#{window_panes}|#{?window_active,1,0}"

// ChooseTree returns a snapshot of the (session, window) tree this
// controller's tmux server currently holds.
//
// `tmux choose-tree` itself is interactive-only in tmux 3.4 (it opens
// a picker inside an attached client), so the snapshot form we expose
// to agents wraps `tmux list-windows` with the appropriate filter
// instead — list-windows happens to print the same fields choose-tree
// would render in its non-interactive `-F` mode on newer tmux builds,
// and works against the headless servers tmux-mcp owns where no
// client is ever attached.
//
// The scope argument selects which slice of the tree to return:
//
//   - "" (empty)         → every window on the server (-a flag).
//   - "session NAME"     → every window of a single session (-t NAME).
//   - "window NAME:WIN"  → just the named window of the named session.
//     `tmux list-windows -t <session>:<window>` is equivalent to
//     `-t <session>` (the window half is ignored), so we list the
//     whole session and filter the matching window in this layer.
//     NAME may be a session name; WIN may be either a window name
//     or a numeric index — both forms compare lexically against
//     #{window_index} and #{window_name}, so tmux's "match either"
//     semantics are preserved.
//
// A scope string in any other shape returns an error before any tmux
// command runs, so a typo on the boundary cannot accidentally be
// interpreted as the unscoped form.
//
// Empty stdout (no windows visible) is treated as a successful empty
// listing — returns (nil, nil) — rather than an error. Callers can
// rely on the zero-length slice for a clean "nothing to show" branch.
//
// A typed errs.ErrSessionNotFound is returned (wrapped) when tmux
// reports the targeted session does not exist, so the JSON-RPC layer
// can map that to CodeSessionNotFound. Other tmux failures pass
// through unchanged so the dispatcher surfaces them via CodeInternal.
func (c *Controller) ChooseTree(ctx context.Context, scope string) ([]ChooseTreeRow, error) {
	args := []string{"list-windows", "-F", chooseTreeFormat}
	scope = strings.TrimSpace(scope)
	// windowFilter is non-empty only for scope="window NAME:WIN":
	// list-windows ignores the window half of `-t` so we need a
	// post-hoc filter to keep the contract that scope=window returns
	// exactly one row (or zero, when the window does not exist).
	var windowFilter string
	switch {
	case scope == "":
		// Server-wide listing — same convention as ListWindows("").
		args = append(args, "-a")
	case strings.HasPrefix(scope, "session "):
		name := strings.TrimSpace(strings.TrimPrefix(scope, "session "))
		if name == "" {
			return nil, errors.New("scope 'session' requires a name")
		}
		args = append(args, "-t", name)
	case strings.HasPrefix(scope, "window "):
		target := strings.TrimSpace(strings.TrimPrefix(scope, "window "))
		if target == "" {
			return nil, errors.New("scope 'window' requires a NAME:WINDOW target")
		}
		// Split on the first ':'; the session half is what we hand
		// to `-t`, and the window half is what we filter against
		// after parsing.
		colon := strings.Index(target, ":")
		if colon < 0 {
			return nil, fmt.Errorf("scope 'window' target %q must be in NAME:WINDOW form", target)
		}
		session := target[:colon]
		win := target[colon+1:]
		if session == "" || win == "" {
			return nil, fmt.Errorf("scope 'window' target %q must be in NAME:WINDOW form", target)
		}
		args = append(args, "-t", session)
		windowFilter = win
	default:
		return nil, fmt.Errorf("unknown scope %q (want \"\", \"session NAME\", or \"window NAME:WINDOW\")", scope)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		// `tmux list-windows -t <session>` rejects an unknown
		// session with "can't find session" or "can't find window"
		// depending on the tmux build. Translate either phrasing
		// into the typed errs.ErrSessionNotFound run() uses
		// elsewhere so the JSON-RPC dispatcher maps every "missing"
		// surface to CodeSessionNotFound uniformly.
		msg := strings.ToLower(err.Error())
		if scope != "" && !errors.Is(err, errs.ErrSessionNotFound) &&
			(strings.Contains(msg, "can't find window") ||
				strings.Contains(msg, "no server running")) {
			return nil, fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		// No windows visible — common when scoped to a session that
		// only just had its last window killed. Return (nil, nil) so
		// callers iterate without a separate empty branch.
		return nil, nil
	}
	lines := strings.Split(out, "\n")
	rows := make([]ChooseTreeRow, 0, len(lines))
	for i, line := range lines {
		row, perr := parseChooseTreeLine(line)
		if perr != nil {
			return nil, fmt.Errorf("choose-tree: line %d: %w", i+1, perr)
		}
		// scope="window NAME:WIN": filter to the named window. The
		// caller may have supplied either a window name or a numeric
		// index — match either path by comparing both, so a target
		// like "demo:0" picks the same row whether tmux calls the
		// window "shell" or anything else.
		if windowFilter != "" {
			if row.WindowName != windowFilter && strconv.Itoa(row.WindowIndex) != windowFilter {
				continue
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// parseChooseTreeLine splits one '|'-delimited row produced by
// chooseTreeFormat into a ChooseTreeRow. The format is fixed at the
// call site (above), so any drift in field count is a bug — reject it
// loudly rather than guess.
func parseChooseTreeLine(line string) (ChooseTreeRow, error) {
	const wantFields = 5
	fields := strings.Split(line, "|")
	if len(fields) != wantFields {
		return ChooseTreeRow{}, fmt.Errorf("expected %d '|'-separated fields, got %d in %q", wantFields, len(fields), line)
	}
	idx, err := strconv.Atoi(strings.TrimSpace(fields[1]))
	if err != nil {
		return ChooseTreeRow{}, fmt.Errorf("window_index %q: %w", fields[1], err)
	}
	panes, err := strconv.Atoi(strings.TrimSpace(fields[3]))
	if err != nil {
		return ChooseTreeRow{}, fmt.Errorf("window_panes %q: %w", fields[3], err)
	}
	// tmux emits "1" for the active window and "0" otherwise.
	active := strings.TrimSpace(fields[4]) == "1"
	return ChooseTreeRow{
		Session:     fields[0],
		WindowIndex: idx,
		WindowName:  fields[2],
		PaneCount:   panes,
		Active:      active,
	}, nil
}

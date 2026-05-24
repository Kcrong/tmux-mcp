package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// NewWindowResult is the structured outcome of a successful
// [Controller.NewWindow] call. Unlike WindowResult (used by the older
// CreateWindow path that pre-dates the structured response surface),
// this carries every identifier tmux assigns to a freshly created
// window so callers don't need to round-trip a list-windows to learn
// the index / id pair. The numeric Index is the same value tmux prints
// for `#{window_index}` (and is what `<session>:<index>` targets resolve
// to); ID carries the `@N` form (`#{window_id}`) which is stable across
// renames and moves and therefore preferable for long-lived references.
type NewWindowResult struct {
	// Session echoes the input session so callers can correlate the
	// response with the request without re-stating the spec.
	Session string
	// Index is the numeric window index (`#{window_index}`) tmux placed
	// the new window at — 0-based, monotonically increasing within the
	// session. Stable enough to use as a target string until the window
	// is moved or another window is inserted before it.
	Index int
	// ID is tmux's stable identifier (`#{window_id}`, e.g. "@7") which
	// survives renames and renumberings. Prefer this form over Index for
	// references that need to outlive layout edits.
	ID string
	// Name is the window name tmux ended up with — either the caller's
	// requested -n value, or whatever tmux auto-assigned when Name was
	// empty (typically the command's basename, e.g. "bash").
	Name string
}

// NewWindow creates a new window inside an existing session via
// `tmux new-window` and returns a fully-populated NewWindowResult. The
// boundary (server tool) is responsible for validating the inputs
// (session/name/command shape); this method just wires the spec into
// the right tmux flags and parses the
// `#{session_name}|#{window_index}|#{window_id}|#{window_name}` line
// that -P/-F prints back into a NewWindowResult.
//
// Parameters:
//   - session: existing tmux session that will host the new window. Required.
//   - name: optional `-n <name>` label. When empty tmux auto-assigns a
//     name from the command's basename.
//   - command: optional initial command to run in the new window
//     (passed as the trailing argv after `--`). When empty tmux runs
//     the user's default shell — same semantics as an interactive
//     `tmux new-window` with no trailing argument.
//   - afterIndex: when >= 0, the new window is inserted *after* that
//     existing index (passed via `-t <session>:<afterIndex>`); when
//     negative the flag is omitted and tmux appends at the next free
//     index. -1 is the "no preference" sentinel because tmux indices
//     are non-negative.
//   - selectWin: when true tmux focuses the new window; when false the
//     `-d` flag suppresses the focus change so the original window
//     remains active.
//
// A missing session surfaces as a wrapped errs.ErrSessionNotFound so
// the JSON-RPC layer maps it to CodeSessionNotFound — the same
// translation CreateWindow / SelectWindow use.
func (c *Controller) NewWindow(
	ctx context.Context,
	session, name, command string,
	afterIndex int,
	selectWin bool,
) (NewWindowResult, error) {
	if session == "" {
		return NewWindowResult{}, errors.New("session required")
	}
	// We use -P to make tmux print the new window's identity, and -F to
	// pin the format so we don't depend on tmux's default "session:index"
	// output (which has changed across versions). The literal '|' is safe
	// because none of the substituted variables can contain it (the
	// boundary validator forbids '|' in user-supplied names).
	args := []string{
		"new-window", "-P",
		"-F", "#{session_name}|#{window_index}|#{window_id}|#{window_name}",
	}
	// When afterIndex >= 0 we ask tmux to insert the new window
	// *after* that existing slot via the `-a -t <session>:<afterIndex>`
	// pair. tmux renumbers any windows past the target up by one to
	// make room. Without `-a`, `-t <session>:<afterIndex>` would mean
	// "create at exactly this index" and fail with "index in use" the
	// instant the slot is occupied — almost never what the agent meant.
	// When afterIndex < 0 we fall back to `-t <session>` (no `-a`) so
	// tmux appends at the next free index, matching the behaviour an
	// interactive `tmux new-window` produces with no positional target.
	if afterIndex >= 0 {
		args = append(args, "-a", "-t", session+":"+strconv.Itoa(afterIndex))
	} else {
		args = append(args, "-t", session)
	}
	if !selectWin {
		// -d means "do not switch to the new window". When selectWin is
		// true we omit the flag so tmux does the usual thing of focusing
		// the new window — same as an interactive `tmux new-window`.
		args = append(args, "-d")
	}
	if name != "" {
		args = append(args, "-n", name)
	}
	if command != "" {
		// tmux new-window treats the trailing args as the shell command
		// (after `--`). Passing it as a single argument is fine; tmux
		// invokes it via /bin/sh -c so quoting / arg-splitting matches
		// what the user would type interactively.
		args = append(args, "--", command)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		// `tmux new-window -t <session>` rejects an unknown session with
		// "can't find window: <session>" because -t names a window
		// target, not a session target. Translate that into the typed
		// errs.ErrSessionNotFound run() emits for "session not found",
		// so the JSON-RPC dispatcher can map it to CodeSessionNotFound
		// the same way CreateWindow does.
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find window") {
			return NewWindowResult{}, fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return NewWindowResult{}, err
	}
	line := strings.TrimRight(out, "\n")
	parts := strings.SplitN(line, "|", 4)
	if len(parts) != 4 {
		return NewWindowResult{}, fmt.Errorf("new-window: unexpected output %q", out)
	}
	idx, perr := strconv.Atoi(parts[1])
	if perr != nil {
		return NewWindowResult{}, fmt.Errorf("new-window: window_index %q: %w", parts[1], perr)
	}
	return NewWindowResult{
		Session: parts[0],
		Index:   idx,
		ID:      parts[2],
		Name:    parts[3],
	}, nil
}

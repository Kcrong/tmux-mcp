package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// SplitOptions describes a single `tmux split-window` invocation. The
// boundary (server tool) does the input validation; this struct just
// shapes the spec into a form the controller can mechanically translate
// into tmux flags.
type SplitOptions struct {
	// Session is the existing tmux session that hosts the pane being
	// split. Required: tmux's split-window without -t would resolve to
	// whatever pane it considers current, which is rarely what the
	// caller meant.
	Session string
	// TargetPane is the pane to split, in tmux's standard target form
	// (e.g. "demo:0.1", "demo:0", "demo"). Empty defaults to splitting
	// the session's currently active pane (`-t <session>`).
	TargetPane string
	// Direction selects the split axis. "horizontal" maps to tmux's
	// `-h` (left/right split) and "vertical" maps to `-v` (top/bottom).
	// Anything else is rejected by the controller — the boundary layer
	// catches the same case earlier with CodeInvalidParams so the JSON-
	// RPC client never sees this code path.
	Direction string
	// Command is the initial command tmux runs in the new pane. Empty
	// falls back to the user's default shell, matching `tmux split-
	// window` with no trailing argument.
	Command string
	// Detach controls focus. true maps to `-d` (don't move focus to the
	// new pane); false (the default) lets tmux follow the new pane the
	// way an interactive split would.
	Detach bool
}

// SplitResult is the structured outcome of a successful SplitPane call.
// Callers (the JSON-RPC layer) format it into the response payload; we
// keep the raw fields here so future shapes can reuse them without
// reparsing tmux output.
type SplitResult struct {
	// ID is the tmux pane identifier (`#{pane_id}`, e.g. "%5"). Stable
	// for the lifetime of the pane and unique across the whole tmux
	// server.
	ID string
	// Index is the 0-based pane index within its window
	// (`#{pane_index}`). Combine with the surrounding "session:window"
	// pair to build a target string for follow-up tools.
	Index int
}

// SplitPane wraps `tmux split-window`. It picks the axis (-h/-v),
// resolves the target (session-only when TargetPane is empty), passes
// through the optional command, and parses tmux's `-PF` output into a
// typed SplitResult so callers can chain pane_select / send_keys
// against the new pane without re-listing the session.
//
// A missing session surfaces as a wrapped errs.ErrSessionNotFound (via
// run() or the can't-find-pane translation below) so the JSON-RPC
// dispatcher maps it to CodeSessionNotFound.
func (c *Controller) SplitPane(ctx context.Context, opts SplitOptions) (SplitResult, error) {
	if opts.Session == "" {
		return SplitResult{}, errors.New("session required")
	}
	flag, err := splitDirectionFlag(opts.Direction)
	if err != nil {
		return SplitResult{}, err
	}
	// -P prints the new pane, -F pins the format so we don't depend on
	// tmux's default (which has changed across versions). The literal
	// '|' is safe: pane_id is "%N" and pane_index is a small integer,
	// neither of which contain pipes.
	args := []string{"split-window", flag, "-P", "-F", "#{pane_id}|#{pane_index}"}
	if opts.Detach {
		// -d means "do not move focus to the new pane"; matches the
		// Detach=true branch so an agent can keep typing into the
		// original pane after the split lands.
		args = append(args, "-d")
	}
	target := opts.Session
	if opts.TargetPane != "" {
		// When the caller pinned a specific pane, pass it verbatim. tmux
		// resolves "session", "session:window", "session:window.pane"
		// uniformly so we leave the format check to the boundary regex.
		target = opts.TargetPane
	}
	args = append(args, "-t", target)
	if opts.Command != "" {
		// `tmux split-window` accepts the command as a trailing argument
		// (it's invoked via /bin/sh -c by tmux), so we pass it as a
		// single argv element to preserve the caller's quoting.
		args = append(args, opts.Command)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		// tmux split-window -t <missing> says "can't find pane" instead
		// of the "session not found" form that run() already maps. Keep
		// the contract: callers should be able to errors.Is into
		// errs.ErrSessionNotFound regardless of the exact message tmux
		// emits.
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find pane") {
			return SplitResult{}, fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return SplitResult{}, err
	}
	line := strings.TrimRight(out, "\n")
	parts := strings.SplitN(line, "|", 2)
	if len(parts) != 2 {
		return SplitResult{}, fmt.Errorf("split-window: unexpected output %q", out)
	}
	idx, perr := strconv.Atoi(parts[1])
	if perr != nil {
		return SplitResult{}, fmt.Errorf("split-window: pane_index %q: %w", parts[1], perr)
	}
	return SplitResult{ID: parts[0], Index: idx}, nil
}

// splitDirectionFlag maps the public Direction string onto the tmux
// flag that selects the split axis. The check is exhaustive so a
// future "tiled" or "auto" value can't slip through silently — the
// controller refuses unknown values up front, keeping symmetry with
// the validateSplitDirection guard in the JSON-RPC layer.
func splitDirectionFlag(direction string) (string, error) {
	switch direction {
	case "horizontal":
		return "-h", nil
	case "vertical":
		return "-v", nil
	default:
		return "", fmt.Errorf("direction %q must be \"horizontal\" or \"vertical\"", direction)
	}
}

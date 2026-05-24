package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PaneInfo is the process-level snapshot returned by
// [Controller.InspectSession]. It deliberately exposes only what tmux
// itself reports for the session's active pane — the foreground process
// PID, its current working directory, and the command name — and never
// the pane's environment, because env vars routinely carry tokens, API
// keys, or other secrets that have no business crossing the JSON-RPC
// boundary.
type PaneInfo struct {
	// PID is the foreground process running in the active pane (i.e.
	// `#{pane_pid}` from tmux). Useful for correlating tmux activity
	// with `ps` output or for kill-on-stuck recovery.
	PID int
	// Cwd is the absolute working directory of the active pane's
	// foreground process (`#{pane_current_path}`).
	Cwd string
	// Command is the foreground command name without arguments
	// (`#{pane_current_command}`, e.g. "bash", "vim", "go").
	Command string
}

// inspectFormat is the format string passed to `tmux display-message`.
// Pieces are separated by '|' which is safe in practice — tmux variables
// for pid / current_path / current_command never contain that
// character. Keeping inspectFormat to one display-message call (rather
// than three) keeps the inspect tool cheap on the hot path.
const inspectFormat = "#{pane_pid}|#{pane_current_path}|#{pane_current_command}"

// InspectSession returns process-level metadata for the active pane of
// the named session: foreground PID, cwd, and command name.
//
// This deliberately complements [Controller.DescribeSession]: describe
// returns session-level metadata (window/pane counts, geometry,
// creation time), inspect returns the active pane's process state.
// Callers debugging "is the shell still alive?" or routing follow-up
// commands based on which tool the user is currently running want
// inspect; callers asking "is this session laid out the way I expect?"
// want describe.
//
// Environment variables are NOT exposed: agents have no reason to need
// them and `#{pane_environment}` would routinely leak tokens / API keys
// across the JSON-RPC boundary. Cross-platform `/proc` reads are also
// avoided so the implementation stays portable to macOS.
//
// Unknown session names surface as a wrapped errs.ErrSessionNotFound
// (via the up-front has-session probe) so the JSON-RPC dispatcher maps
// them to CodeSessionNotFound — without the probe, tmux's
// display-message would just emit a blank line for an unknown target
// and we would have to guess at the parse error's cause.
func (c *Controller) InspectSession(ctx context.Context, name string) (PaneInfo, error) {
	if name == "" {
		return PaneInfo{}, errors.New("session name required")
	}
	// has-session is the canonical existence check — its stderr
	// ("can't find session: <name>") is already recognised by
	// isSessionMissingMsg, so the wrapping happens automatically inside
	// run(). Doing this up front means the rest of this method can
	// assume the session exists.
	if _, err := c.run(ctx, "has-session", "-t", name); err != nil {
		return PaneInfo{}, err
	}
	// :0.0 anchors the format expansion to a real window+pane so the
	// pane_* variables resolve. Without an explicit pane target, some
	// tmux versions evaluate the variables against the empty "current"
	// client and return blanks even though the session exists.
	out, err := c.run(ctx, "display-message", "-p", "-t", name+":0.0", "-F", inspectFormat)
	if err != nil {
		return PaneInfo{}, err
	}
	parts := strings.Split(strings.TrimRight(out, "\n"), "|")
	if len(parts) != 3 {
		return PaneInfo{}, fmt.Errorf("inspect %q: unexpected display-message output %q", name, out)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return PaneInfo{}, fmt.Errorf("inspect %q: parse pane_pid %q: %w", name, parts[0], err)
	}
	cwd := strings.TrimSpace(parts[1])
	command := strings.TrimSpace(parts[2])
	return PaneInfo{
		PID:     pid,
		Cwd:     cwd,
		Command: command,
	}, nil
}

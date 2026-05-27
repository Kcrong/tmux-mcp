package tmuxctl

import (
	"context"
	"errors"
	"fmt"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// PasteBuffer wraps `tmux paste-buffer [-d] [-p] [-b NAME] -t TARGET`.
// It instructs tmux to inject the bytes of a stored paste buffer into
// the targeted pane, exactly as if the user had hit the configured
// paste key — useful for an MCP agent that staged a snippet via
// SetBuffer (or via the set_buffer tool) and now wants to deliver it
// into a running shell or TUI without paying the per-keystroke cost
// of send-keys.
//
// Argument semantics:
//
//   - target is required and must be a tmux pane-target string
//     ("session", "session:window", or "session:window.pane"). The
//     boundary regex catches stray quoting / shell metachars before we
//     reach this layer; tmuxctl itself only enforces the required-arg
//     contract and the typed missing-buffer / missing-session sentinels.
//   - name is optional. When empty, tmux pastes the most-recently
//     added buffer (the same default the bare `paste-buffer` CLI form
//     applies). When non-empty, `-b NAME` pins the buffer being pasted.
//   - deleteAfter maps to tmux's `-d` flag. When true, tmux deletes the
//     buffer from its in-memory list after the paste completes — the
//     idiomatic "use once and discard" pattern for ephemeral
//     clipboard-style snippets that the agent does not want to leak
//     across subsequent list_buffers calls.
//   - bracketed maps to tmux's `-p` flag. When true (and the receiving
//     application has signalled it understands bracketed paste mode),
//     tmux brackets the paste with the standard escape sequences so a
//     shell / editor can distinguish typed input from pasted bytes.
//     Forwarding the flag verbatim — rather than e.g. inferring it
//     from the target — keeps the boundary thin and lets the caller
//     decide based on what the application actually supports.
//
// Error handling. A genuine "no buffer" stderr from tmux is mapped
// onto the typed errs.ErrSessionNotFound sentinel via isBufferMissingMsg,
// matching the contract ShowBuffer already establishes for the read-side
// surface: the JSON-RPC dispatcher then maps it to CodeSessionNotFound
// (-32000) so MCP clients can branch on a stable wire code regardless of
// which exact phrase the local tmux version emitted ("no buffer X" vs
// the older "unknown buffer" message). A missing pane / session passes
// through unchanged because run() already wraps that flavour of stderr
// onto ErrSessionNotFound — both diagnostics end up sharing the same
// outcome for the agent, which is the right reduction (the agent only
// cares that "the named thing does not exist").
func (c *Controller) PasteBuffer(ctx context.Context, target, name string, deleteAfter, bracketed bool) error {
	if target == "" {
		return errors.New("target required")
	}
	args := []string{"paste-buffer"}
	if deleteAfter {
		args = append(args, "-d")
	}
	if bracketed {
		args = append(args, "-p")
	}
	if name != "" {
		args = append(args, "-b", name)
	}
	args = append(args, "-t", target)
	if _, err := c.run(ctx, args...); err != nil {
		// run() already maps "session not found" stderr to the typed
		// sentinel; here we additionally translate the buffer-specific
		// "no buffer NAME" / "unknown buffer" phrasings so callers can
		// errors.Is into errs.ErrSessionNotFound regardless of which
		// branch tmux took. Reusing ErrSessionNotFound (instead of
		// minting a parallel sentinel) keeps the wire contract simple:
		// the JSON-RPC layer already maps it to a stable -32000 code,
		// and MCP clients can branch on that single outcome for "the
		// named thing does not exist on this server".
		if !errors.Is(err, errs.ErrSessionNotFound) && isBufferMissingMsg(err.Error()) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// RespawnWindow wraps `tmux respawn-window [-k] [-c <cwd>] -t
// <session>:<window> [shell-command]`. It is the window-scoped sibling
// of [Controller.RespawnPane] — instead of re-running the command in a
// single pane, tmux respawns the *whole window*, replacing whatever
// command (or `remain-on-exit` corpse) was there. Useful when a
// long-running window-level process (e.g. a build watcher that owned
// the only pane in its window) has exited and the agent wants to bring
// it back without recreating the window and disturbing the surrounding
// session layout.
//
// The boundary builds the full `session:window` target string from the
// caller-supplied components so each piece picks up regex / length
// validation at the server layer, instead of one freeform "target" knob
// with the easy-to-misuse semantics tmux's CLI inherits.
//
// command is the optional shell command to start in the respawned
// window. When empty, tmux re-runs whatever the window was created (or
// last respawned) with — matching the documented `respawn-window`
// behaviour from the tmux man page. When non-empty, the string is
// forwarded as a single trailing argv to tmux respawn-window; tmux
// invokes it via /bin/sh -c so shell quoting rules apply on the tmux
// side.
//
// cwd, when non-empty, sets the new starting directory for the window
// via tmux's `-c <start-directory>` flag. This is the natural choice
// when the original directory has been removed or renamed and a plain
// respawn would fail to start the new command.
//
// kill toggles the `-k` flag. tmux respawn-window's default behaviour
// is to refuse the call when the original command is still running,
// with stderr "window <target> still active". `-k` opts into the
// kill-then-respawn semantics (tmux SIGKILLs the running process before
// starting the new one). We surface the "still active" branch as the
// existing errs.ErrPaneActive sentinel — the same one
// [Controller.RespawnPane] uses — so the JSON-RPC layer maps it to the
// stable CodePaneActive (-32005) clients already branch on. Reusing
// the sentinel keeps the recovery contract uniform across both respawn
// tools: clients detect "busy" once and retry with kill=true regardless
// of whether the call was pane- or window-scoped.
//
// session and window are both required: respawn-window operates on a
// specific window, and tmux resolves an empty `-t` to whatever window
// it considers current — almost never what the caller actually wanted.
// The component validation lives at the server tool boundary (see
// internal/server/tools_respawn_window.go); this controller method
// only enforces the bare-minimum non-empty contract so a unit test
// that calls it directly fails loudly instead of silently respawning
// the wrong window.
//
// A missing session/window surfaces as a wrapped errs.ErrSessionNotFound
// so the JSON-RPC dispatcher maps it to CodeSessionNotFound — the same
// contract every other window-scoped tmuxctl method upholds. tmux
// phrases the missing-target case as "can't find window" / "can't find
// session" depending on which component is absent and which version is
// on PATH; translate the window variant into the same typed sentinel
// run() emits for "session not found".
func (c *Controller) RespawnWindow(ctx context.Context, session, window, command, cwd string, kill bool) error {
	if session == "" {
		return errors.New("session required")
	}
	if window == "" {
		return errors.New("window required")
	}
	target := fmt.Sprintf("%s:%s", session, window)
	args := []string{"respawn-window"}
	if kill {
		// -k must precede -t / shell-command. tmux is order-tolerant
		// here in practice, but mirroring the man page argv order keeps
		// the emitted command line readable in audit logs.
		args = append(args, "-k")
	}
	if cwd != "" {
		// -c sets the new starting directory. tmux passes the value to
		// fork()/chdir() before exec, so an absolute path is the only
		// shape that consistently works across tmux versions; the
		// boundary validator already enforces that.
		args = append(args, "-c", cwd)
	}
	args = append(args, "-t", target)
	if command != "" {
		// Forward command as a single trailing argv. tmux's man page
		// describes `[shell-command]` as the optional last positional
		// argument, and respawn-window wraps it in /bin/sh -c on the
		// tmux side — we deliberately do not pre-quote it here so a
		// command like `sh -c "echo hi"` lands at tmux exactly the way
		// the caller wrote it. The server boundary already rejects
		// newlines so a single argv entry maps to a single shell line.
		args = append(args, command)
	}
	if _, err := c.run(ctx, args...); err != nil {
		msg := strings.ToLower(err.Error())
		// tmux phrases the busy-window case as "window <target> still
		// active". Reuse the same errs.ErrPaneActive sentinel respawn-pane
		// surfaces so the JSON-RPC dispatcher returns CodePaneActive —
		// clients already branch on the code and retry with kill=true,
		// and minting a new sentinel just for window-scope would force
		// every recovery path to handle two codes that mean the same
		// thing.
		if strings.Contains(msg, "still active") {
			return fmt.Errorf("respawn-window %s: %s: %w", target, err.Error(), errs.ErrPaneActive)
		}
		// Same translation we apply elsewhere for "can't find window":
		// run() only wraps "session not found" directly, but the "wrong
		// window on a real session" branch shares the same MCP-level
		// meaning (the addressed target does not exist).
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			(strings.Contains(msg, "can't find window") ||
				strings.Contains(msg, "no current target")) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

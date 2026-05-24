package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// RespawnPane wraps `tmux respawn-pane [-k] -t <session>:<window>.<pane>
// [shell-command]`. The boundary builds the full target string from the
// caller-supplied session / window / pane components so callers get a
// uniform validation layer (each piece is regex-checked at the server)
// instead of one freeform "target" knob with the easy-to-misuse semantics
// tmux's CLI inherits.
//
// command is the optional shell command to start in the respawned pane.
// When empty, tmux re-runs whatever the original pane was created with
// (the user's default shell, or whatever shell-command arg the original
// session/window/pane carried) — this matches the documented `respawn-pane`
// behaviour and is the natural choice for "the build crashed, just bring
// it back". When non-empty, command is forwarded as a single trailing argv
// to tmux respawn-pane; tmux executes it via /bin/sh -c so shell quoting
// rules apply on the tmux side.
//
// kill toggles the `-k` flag. tmux respawn-pane's default behaviour is
// to refuse the call when the original command is still running, with
// stderr "pane <target> still active". `-k` opts into the kill-then-
// respawn semantics (tmux SIGKILLs the running process before starting
// the new one). We surface the "still active" branch as
// errs.ErrPaneActive so the JSON-RPC layer can map it to a stable code
// (CodePaneActive, -32005) the client can branch on to retry with
// kill=true — substring-matching the message would tie clients to the
// exact phrasing tmux uses.
//
// session, window, pane are all required: respawn-pane operates on a
// specific pane, and tmux resolves an empty `-t` to whatever pane it
// considers current — almost never what the caller actually wanted. The
// component validation lives at the server tool boundary (see
// internal/server/tools_respawn_pane.go); this controller method only
// enforces the bare-minimum non-empty contract so a unit test that calls
// it directly fails loudly instead of silently respawning the wrong pane.
//
// A missing session/window/pane surfaces as a wrapped errs.ErrSessionNotFound
// so the JSON-RPC dispatcher maps it to CodeSessionNotFound — the same
// contract every other pane-scoped tmuxctl method upholds. tmux phrases
// the missing-target case as "can't find pane" / "can't find window" /
// "can't find session" depending on which component is absent and which
// version is on PATH; translate the pane/window variants into the same
// typed sentinel run() emits for "session not found".
func (c *Controller) RespawnPane(ctx context.Context, session, window, pane, command string, kill bool) error {
	if session == "" {
		return errors.New("session required")
	}
	if window == "" {
		return errors.New("window required")
	}
	if pane == "" {
		return errors.New("pane required")
	}
	target := fmt.Sprintf("%s:%s.%s", session, window, pane)
	args := []string{"respawn-pane"}
	if kill {
		// -k must precede -t / shell-command. tmux is order-tolerant
		// here in practice, but mirroring the man page argv keeps the
		// emitted command line readable in audit logs.
		args = append(args, "-k")
	}
	args = append(args, "-t", target)
	if command != "" {
		// Forward command as a single trailing argv. tmux's man page
		// describes `[shell-command]` as the optional last positional
		// argument, and respawn-pane wraps it in /bin/sh -c on the
		// tmux side — we deliberately do not pre-quote it here so a
		// command like `sh -c "echo hi"` lands at tmux exactly the way
		// the caller wrote it. The server boundary already rejects
		// newlines so a single argv entry maps to a single shell line.
		args = append(args, command)
	}
	if _, err := c.run(ctx, args...); err != nil {
		msg := strings.ToLower(err.Error())
		// tmux phrases the busy-pane case as "pane <target> still
		// active". Surface that as the typed ErrPaneActive sentinel so
		// the JSON-RPC dispatcher can return CodePaneActive — clients
		// branch on the code and retry with kill=true.
		if strings.Contains(msg, "still active") {
			return fmt.Errorf("respawn-pane %s: %s: %w", target, err.Error(), errs.ErrPaneActive)
		}
		// Same translation we apply elsewhere for "can't find pane" /
		// "can't find window": run() only wraps "session not found"
		// directly, but the "wrong pane on a real session" or "wrong
		// window on a real session" branches share the same MCP-level
		// meaning (the addressed target does not exist).
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			(strings.Contains(msg, "can't find pane") ||
				strings.Contains(msg, "can't find window") ||
				strings.Contains(msg, "no current target")) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

package tmuxctl

import (
	"context"
	"errors"
)

// IfShell wraps `tmux if-shell [-bF] SHELL_COMMAND TMUX_COMMAND
// [ELSE_TMUX_COMMAND]`. tmux runs SHELL_COMMAND through `/bin/sh -c` (or
// substitutes #{format} expansion when -F is set) and dispatches
// thenCommand on success (exit 0) or elseCommand on failure (any other
// exit). This is the canonical "conditional" surface the agent reaches
// for when it wants tmux to take action only if a process is healthy
// (e.g. "if pgrep -x build-watch; then display-message running; else
// display-message stopped").
//
// shellCommand is forwarded as a single positional argv entry — the same
// way tmux's own command line accepts it — so a multi-token shell
// pipeline (`pgrep -x build && wc -l file`) lands at tmux exactly as the
// caller wrote it. The same applies to thenCommand and elseCommand:
// tmux parses each as a tmux command line on its own, so the boundary
// layer is responsible for length / control-char hygiene up front. The
// COMMAND ITSELF IS NOT SANDBOXED by us — that is a documented
// operator-trust boundary; the boundary layer rejects NUL/control bytes
// and bounds the payload size before this method runs.
//
// background maps to `-b`: tmux runs the shell command in a detached
// child process; the call returns immediately and tmux dispatches the
// chosen branch only after the shell command exits. When background is
// false (the default) tmux blocks until the shell command finishes,
// matching the synchronous "if/else" semantics most agents want.
//
// formatExpand maps to `-F`: tmux interprets shellCommand as a
// `#{format}` expression instead of running it through /bin/sh. A
// non-empty expansion result counts as success (then-branch);
// empty/zero/false counts as failure (else-branch). Useful for
// conditional dispatch that only inspects tmux state (e.g.
// `#{==:#{session_name},build}`) without paying for a fork+exec.
//
// Unlike most other tmuxctl methods, if-shell does NOT take `-t`, so a
// missing-target diagnostic from a stray TMUX_COMMAND surfaces as the
// generic tmux error (e.g. "syntax error" / "unknown command"). We
// deliberately do not translate those into errs.ErrSessionNotFound —
// they are legitimate failures the caller wants to debug, not session
// drift the dispatcher should remap.
func (c *Controller) IfShell(ctx context.Context, shellCommand, thenCommand, elseCommand string, background, formatExpand bool) error {
	if shellCommand == "" {
		return errors.New("shell_command required")
	}
	if thenCommand == "" {
		return errors.New("then_command required")
	}
	args := []string{"if-shell"}
	if background {
		// -b runs SHELL_COMMAND in a detached child. The call returns
		// immediately and tmux dispatches the chosen branch only after
		// the shell command exits — useful for fire-and-forget probes
		// that should not block the agent's tools/call.
		args = append(args, "-b")
	}
	if formatExpand {
		// -F treats SHELL_COMMAND as a tmux #{format} expression instead
		// of running it through /bin/sh -c. tmux then evaluates the
		// expansion; non-empty/non-zero/non-"0" counts as success.
		args = append(args, "-F")
	}
	// shellCommand and thenCommand are required positional args; tmux
	// parses each as a single argv entry on its own (the boundary
	// already rejected NUL/control bytes that would split the frame).
	args = append(args, shellCommand, thenCommand)
	if elseCommand != "" {
		// elseCommand is the optional third positional arg. tmux
		// dispatches it on the failure branch only when the shell
		// command exited non-zero (or the format expansion was empty).
		args = append(args, elseCommand)
	}
	if _, err := c.run(ctx, args...); err != nil {
		return err
	}
	return nil
}

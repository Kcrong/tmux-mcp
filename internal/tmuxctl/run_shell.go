package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// RunShell wraps `tmux run-shell [-b] [-c <start-dir>] [-t <target>]
// <shell-command>`. Unlike PipePane (which hooks a long-running shell
// onto a pane's I/O so every byte tmux writes flows through it) and
// SendKeys (which types into a pane and lets the running process see
// the input), run-shell executes a one-shot shell command on the tmux
// server host, outside any pane, and is the canonical hook for
// fire-and-forget side effects (logging, notifications, dispatching
// helper scripts) that should never disturb the foreground pane.
//
// command is the shell pipeline tmux will run via /bin/sh. tmux's own
// run-shell does NOT pipe stdout back to the calling client — it writes
// the command's output to view-mode in the active pane, where a calling
// agent cannot read it. To honour the documented "returns the captured
// stdout" contract this method redirects the command's stdout/stderr
// into a private temp file (path stays under os.TempDir, controlled by
// os.CreateTemp so the chars are alnum-safe) and reads it back after
// tmux finishes. The temp file is removed before this method returns,
// success or failure, so the controller never leaks state on the host
// even if the caller cancels mid-call.
//
// startDir, when non-empty, is forwarded as `-c <start-dir>` so tmux
// chdir's into it before exec'ing /bin/sh; the caller is responsible
// for absolute-path / existence validation up at the boundary.
//
// target, when non-empty, is forwarded as `-t <target>`. tmux
// run-shell silently ignores an invalid -t (the command runs anyway
// against tmux's current/global context), so we run an explicit
// `has-session -t <target>` probe up front whenever a target is
// supplied; its stderr ("can't find session: ...") is already
// recognised by run()'s isSessionMissingMsg, which wraps it into the
// typed errs.ErrSessionNotFound sentinel. Without this probe a missing
// target would silently produce a "successful" run with no output,
// which is exactly the surprising behaviour the typed sentinel exists
// to prevent.
//
// background=true maps to `-b`. tmux returns immediately, the command
// runs detached, and stdout is intentionally discarded — the temp-file
// dance does not apply, and the method returns ("", nil) on success.
// Use this for fire-and-forget notifications / chained tmux hooks
// where the agent does not want to block on the side effect.
//
// CAUTION: this method runs ARBITRARY shell commands on the controller
// host. The shell command itself is not sandboxed — operators must
// trust the agents that can call this method. The same trust model
// gating PipePane applies; the boundary layer is responsible for
// argument-shape hygiene (length / control-char / UTF-8) up front.
func (c *Controller) RunShell(ctx context.Context, command, startDir, target string, background bool) (string, error) {
	if command == "" {
		return "", errors.New("command required")
	}
	if target != "" {
		// has-session is the canonical existence check — its stderr
		// ("can't find session: <name>") is recognised by
		// isSessionMissingMsg inside run(), so the wrapping into
		// errs.ErrSessionNotFound happens automatically. Doing this up
		// front means the rest of this method can assume the target
		// exists, which matters because `tmux run-shell -t <missing>`
		// silently runs the command anyway against tmux's
		// current/global context — exactly the surprising behaviour
		// the typed sentinel exists to prevent. We deliberately probe
		// only the bare session name (split on ':') because tmux's
		// has-session does not accept window/pane suffixes, but the
		// session existence is the load-bearing pre-condition; window
		// drift on a real session falls back to tmux's own silent
		// behaviour, matching the spec's "session not found" focus.
		session := target
		if i := strings.IndexAny(session, ":."); i >= 0 {
			session = session[:i]
		}
		if _, err := c.run(ctx, "has-session", "-t", session); err != nil {
			return "", err
		}
	}
	if background {
		// -b is fire-and-forget: tmux returns immediately, the command
		// runs detached, and the documented contract is that stdout is
		// discarded. We forward command verbatim (no temp-file capture
		// wrapper) so the caller's pipeline lands at /bin/sh exactly
		// the way they wrote it.
		args := []string{"run-shell", "-b"}
		if startDir != "" {
			args = append(args, "-c", startDir)
		}
		if target != "" {
			args = append(args, "-t", target)
		}
		args = append(args, command)
		if _, err := c.run(ctx, args...); err != nil {
			return "", wrapRunShellMissingTarget(err)
		}
		return "", nil
	}
	// Synchronous path: tmux run-shell does not pipe stdout back to
	// the client (output goes to view-mode in the active pane), so we
	// stage a private temp file and rewrite the user's command into a
	// brace-grouped redirect: `{ <cmd>; } >'<tmpfile>' 2>&1`. The
	// temp-file path comes from os.CreateTemp under os.TempDir, so the
	// path chars are alnum/underscore-safe and the single-quote
	// wrapper is sufficient — the boundary layer already rejects NUL
	// / control bytes / non-UTF-8 in command, so the user payload
	// can't smuggle a closing quote either.
	tmp, err := os.CreateTemp("", "tmux-mcp-runshell-*.out")
	if err != nil {
		return "", fmt.Errorf("run_shell: stage capture file: %w", err)
	}
	tmpPath := tmp.Name()
	// Close immediately — we only need the name; tmux's child shell
	// will reopen it through the redirect. Defer file removal so we
	// never leak host state, even if the caller cancels mid-call.
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	wrapped := "{ " + command + "; } >'" + tmpPath + "' 2>&1"
	args := []string{"run-shell"}
	if startDir != "" {
		args = append(args, "-c", startDir)
	}
	if target != "" {
		args = append(args, "-t", target)
	}
	args = append(args, wrapped)
	if _, runErr := c.run(ctx, args...); runErr != nil {
		// tmux propagates the user shell's exit code: a non-zero exit
		// from the wrapped command surfaces here as a non-sentinel
		// run() error (the stderr text comes from tmux's own argv
		// parser, not from the shell — the shell's diagnostics already
		// landed in the temp file via 2>&1). Surface it as-is so the
		// JSON-RPC layer maps it to CodeInternal — "the call ran but
		// the user command failed" is a legitimate non-session error.
		// Missing-target wrapping already happened above via the
		// has-session probe, so we don't re-wrap here.
		return "", fmt.Errorf("run_shell: %w", runErr)
	}
	body, readErr := os.ReadFile(tmpPath) //nolint:gosec // path is os.CreateTemp-rooted, controlled by this method
	if readErr != nil {
		return "", fmt.Errorf("run_shell: read capture: %w", readErr)
	}
	return string(body), nil
}

// wrapRunShellMissingTarget translates the alternative phrasings tmux's
// run-shell occasionally emits ("can't find pane", "no current target")
// into the typed errs.ErrSessionNotFound sentinel, so the JSON-RPC
// dispatcher maps every "target does not resolve" variant onto
// CodeSessionNotFound regardless of which form tmux happened to use.
// The typical session-missing case is already wrapped by the
// has-session probe in RunShell; this helper only catches the rarer
// background-path variants where the probe path doesn't fire (e.g. -b
// with a target that resolved through has-session but was killed
// between the probe and the run-shell exec).
func wrapRunShellMissingTarget(err error) error {
	if err == nil || errors.Is(err, errs.ErrSessionNotFound) {
		return err
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "can't find pane") ||
		strings.Contains(msg, "no current target") ||
		strings.Contains(msg, "can't find window") {
		return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
	}
	return err
}

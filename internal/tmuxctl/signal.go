package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// signalTable is the whitelist of POSIX signal names accepted by
// SendSignal. Only the signals an agent realistically needs for
// process control are exposed — keeping the surface tight avoids the
// trap of letting a caller fire SIGSTOP / SIGTRAP / SIGSEGV at the
// pane process and pretend that's a normal request.
//
// The table is exported (lowercase keys exposed via SignalNames) so
// the JSON-RPC layer can build its enum and error messages from the
// same source of truth, instead of hard-coding the list in two places.
//
// The map itself is defined per-platform (signal_unix.go /
// signal_windows.go) because syscall.SIGUSR1 / SIGUSR2 are POSIX-only
// and the Windows toolchain rejects them at compile time. Windows
// users still see USR1/USR2 in SignalNames() so the wire protocol /
// JSON-RPC schema is identical across platforms; SendSignal then
// returns a platform-specific "not supported" error if they're
// actually requested at runtime.

// SignalNames returns the accepted signal names in a deterministic
// order so callers (the tool schema, error messages) can render them
// consistently across invocations.
//
// The list is identical on every platform so the JSON-RPC schema does
// not become OS-dependent — Windows simply rejects USR1/USR2 with a
// friendly error inside SendSignal instead of returning a different
// enum.
func SignalNames() []string {
	return []string{"TERM", "HUP", "INT", "QUIT", "USR1", "USR2", "KILL"}
}

// resolveSignal looks up the os.Signal that backs a whitelisted name.
// The lookup is case-sensitive (uppercase) so `SIGTERM`, `term`, and
// stray whitespace are rejected up front rather than silently matching.
func resolveSignal(name string) (os.Signal, bool) {
	sig, ok := signalTable[name]
	return sig, ok
}

// SendSignal delivers a POSIX signal to the PID of the session's
// currently active pane. It is a more precise alternative to
// SendKeys with "C-c" / "C-\\" because:
//
//   - The signal targets the foreground process group leader of the
//     pane (whatever pane_pid points at), which is exactly the
//     program the user is running — not whatever happens to be
//     interpreting the keystroke at the time.
//   - SIGTERM / SIGKILL still work even when the program has stolen
//     the keyboard (raw mode TUIs, daemons that swallow Ctrl-C, …).
//
// signal must be one of the names returned by SignalNames; anything
// else is rejected with an error so the JSON-RPC layer can map that
// to CodeInvalidParams.
//
// A missing session surfaces errs.ErrSessionNotFound (wrapped) so the
// dispatcher returns CodeSessionNotFound instead of the generic
// internal-error code.
func (c *Controller) SendSignal(ctx context.Context, session, signal string) error {
	if session == "" {
		return errors.New("session required")
	}
	// platformRejectSignal lets the Windows build refuse SIGUSR1 /
	// SIGUSR2 with a friendly message — those constants are POSIX-only
	// so they can't sit in signalTable on Windows, and we want the
	// caller to see "not supported on Windows" rather than a generic
	// "not in whitelist" that contradicts SignalNames().
	if err := platformRejectSignal(signal); err != nil {
		return err
	}
	sig, ok := resolveSignal(signal)
	if !ok {
		return fmt.Errorf("signal %q not in whitelist %v", signal, SignalNames())
	}
	// Pre-check that the session exists. tmux's display-message has a
	// quirky failure mode where naming a nonexistent session prints an
	// empty line and exits 0, swallowing the error. Going through
	// HasSession (which calls list-sessions under the hood) gives us a
	// reliable, typed errs.ErrSessionNotFound for missing names so the
	// JSON-RPC layer can map it to CodeSessionNotFound.
	has, err := c.HasSession(ctx, session)
	if err != nil {
		return fmt.Errorf("look up session %q: %w", session, err)
	}
	if !has {
		return fmt.Errorf("session %q: %w", session, errs.ErrSessionNotFound)
	}
	// `display-message -p -t <session>:0.0 '#{pane_pid}'` returns the
	// PID of the pane that is currently active for the session's first
	// window. We deliberately do not let the caller pin a pane index
	// here — the tool surface today operates per-session, and pane
	// targeting belongs to the pane_select tool (see internal/server/
	// tools_panes.go).
	out, err := c.run(ctx, "display-message", "-p", "-t", session+":0.0", "#{pane_pid}")
	if err != nil {
		// run() already wraps errs.ErrSessionNotFound for missing
		// sessions; preserve that so callers can errors.Is into it.
		return err
	}
	pidStr := strings.TrimSpace(out)
	if pidStr == "" {
		return fmt.Errorf("tmux returned empty pane_pid for session %q", session)
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return fmt.Errorf("parse pane_pid %q: %w", pidStr, err)
	}
	// os.FindProcess + Process.Signal is the cross-platform Go way to
	// deliver a signal. On Unix it boils down to syscall.Kill(pid, sig);
	// FindProcess never returns an error here because POSIX semantics
	// are "always succeeds" — the real failure (no such process,
	// permission denied) shows up at Signal time.
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := proc.Signal(sig); err != nil {
		return fmt.Errorf("signal pid %d with %s: %w", pid, signal, err)
	}
	return nil
}

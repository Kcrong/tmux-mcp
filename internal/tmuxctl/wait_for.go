package tmuxctl

import (
	"context"
	"errors"
	"fmt"
)

// WaitForMode selects which variant of `tmux wait-for` to dispatch.
// tmux's wait-for is a single command with three mutually-exclusive
// flags (-L lock, -S signal, -U unlock) plus the no-flag "wait until
// signalled" form; modelling them as an enum here is more honest than
// a bag of booleans because the controller boundary cannot meaningfully
// combine two of them in a single tmux call.
type WaitForMode int

// WaitFor* enumerate the four shapes of `tmux wait-for`.
const (
	// WaitForWait blocks until another caller fires `wait-for -S` against
	// the same channel. Maps to bare `tmux wait-for CHANNEL` with no
	// flags. Honours ctx — the caller is expected to attach a timeout
	// via context.WithTimeout when an unbounded block is undesirable, as
	// tmux itself imposes no upper bound on how long it will wait.
	WaitForWait WaitForMode = iota
	// WaitForLock acquires the named channel as a mutex. If another
	// caller already holds the lock, blocks until they release it via
	// `wait-for -U`. Maps to `tmux wait-for -L CHANNEL`. Like
	// WaitForWait, the ctx is the only deadline tmux observes.
	WaitForLock
	// WaitForSignal wakes every caller currently blocked on
	// `wait-for CHANNEL` (the WaitForWait branch). Returns immediately;
	// when no waiters exist tmux is happy to "signal nothing" and
	// returns success without buffering — the signal is lost. Maps to
	// `tmux wait-for -S CHANNEL`.
	WaitForSignal
	// WaitForUnlock releases a channel previously acquired via
	// WaitForLock so the next blocked locker can proceed. Returns
	// immediately. Maps to `tmux wait-for -U CHANNEL`. tmux returns an
	// error when called against a channel that is not currently locked,
	// which surfaces verbatim through run().
	WaitForUnlock
)

// String renders the mode as the tmux flag it maps to (or "(wait)" for
// the no-flag form), making error / log messages line up with the
// argv shape the controller actually dispatched.
func (m WaitForMode) String() string {
	switch m {
	case WaitForWait:
		return "(wait)"
	case WaitForLock:
		return "-L"
	case WaitForSignal:
		return "-S"
	case WaitForUnlock:
		return "-U"
	}
	return fmt.Sprintf("WaitForMode(%d)", int(m))
}

// WaitFor wraps `tmux wait-for [-L|-S|-U] CHANNEL`. The channel is the
// rendezvous identifier — tmux treats it as an opaque string, so the
// boundary (server tool) is responsible for the up-front
// regex/length check; the controller passes the value verbatim to tmux.
//
// Mode selection. The four legal shapes are documented on the
// WaitForMode constants:
//
//   - WaitForWait   → bare `wait-for CHANNEL` (BLOCKING — see below).
//   - WaitForLock   → `wait-for -L CHANNEL` (BLOCKING when contended).
//   - WaitForSignal → `wait-for -S CHANNEL` (returns immediately).
//   - WaitForUnlock → `wait-for -U CHANNEL` (returns immediately).
//
// Blocking contract. WaitForWait and (when the lock is held) WaitForLock
// will block tmux indefinitely until somebody else fires the
// corresponding `-S` / `-U` against the same channel. The controller
// dispatches through [Controller.run], which itself dispatches through
// `exec.CommandContext`, so cancelling ctx (a deadline expiring or the
// caller's `cancel()` running) terminates the tmux child process and
// surfaces as the wrapped context error from run(). Callers must wire
// a deadline into ctx — there is no sentinel timeout the wrapper
// imposes on its own, and tmux silently waiting forever is almost
// always not what an LLM agent meant.
//
// Error mapping is deliberately thin: any tmux failure flows through
// run() unchanged. tmux phrases "channel is not locked" (when -U is
// fired against an idle channel) as a generic rc=1 with stderr
// "channel not locked", which the JSON-RPC layer maps to CodeInternal
// (-32603) — operators see the verbatim message and can debug from
// there. We deliberately do not synthesise a typed sentinel for the
// not-locked case because callers have not asked for it; future work
// can add one if a higher-level error code becomes useful.
func (c *Controller) WaitFor(ctx context.Context, mode WaitForMode, channel string) error {
	if channel == "" {
		return errors.New("channel required")
	}
	args := []string{"wait-for"}
	switch mode {
	case WaitForWait:
		// no flag — bare `tmux wait-for CHANNEL`
	case WaitForLock:
		args = append(args, "-L")
	case WaitForSignal:
		args = append(args, "-S")
	case WaitForUnlock:
		args = append(args, "-U")
	default:
		return fmt.Errorf("wait-for: unknown mode %d", int(mode))
	}
	args = append(args, channel)
	if _, err := c.run(ctx, args...); err != nil {
		return err
	}
	return nil
}

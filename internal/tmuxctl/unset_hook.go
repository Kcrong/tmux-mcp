package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// UnsetHook wraps `tmux set-hook -u [-g | -w] [-t TARGET] HOOK-NAME` —
// the inverse of SetHook's bind path. It clears whatever command is
// currently bound to the named hook event in the targeted scope.
// The boundary layer (server-tool) is responsible for validating the
// regex/length shape of `hookName`; this method passes it through to
// tmux verbatim and is the single place argv assembly lives so a
// future maintainer changing the flag order only has to touch one
// spot.
//
// Argument flavours:
//
//   - `global=true` clears the hook on the server-wide options table
//     (`-g`), which is the inverse of a `SetHook(..., global=true)`
//     bind. `target` is ignored on this path because `-g` and `-t`
//     are mutually exclusive on tmux's argv.
//   - `window=true` clears the hook on the window options table
//     (`-w`). `target` may name a specific window (`-w -t TARGET`)
//     or be empty for the current window. `global` and `window` are
//     mutually exclusive — the boundary refuses the contradiction up
//     front so callers see a clean argument-shape error rather than
//     tmux's version-dependent stderr.
//   - Otherwise the hook is cleared on the per-session options of the
//     resolved `target` session (`-t TARGET`). `target` must be
//     non-empty in this branch — without it tmux would resolve "" to
//     whatever session it considers current, which is almost never
//     what the caller actually wanted, and is mapped here to
//     errs.ErrSessionNotFound so the JSON-RPC layer surfaces a stable
//     CodeSessionNotFound (-32000) instead of tmux's "no current
//     target" stderr.
//
// Error mapping:
//
//   - missing target session: surfaced via run() as a wrapped
//     errs.ErrSessionNotFound (the underlying message is
//     "can't find session: <target>" which isSessionMissingMsg
//     already recognises) so the JSON-RPC dispatcher maps it to
//     CodeSessionNotFound. We also catch tmux's "no such window" /
//     "invalid option" / "no current target" stderr shapes the
//     unset path can produce on older tmux releases and fold them
//     into the same sentinel so callers get one code regardless of
//     which exact phrase tmux emitted.
//   - empty hook name: rejected up front as a plain error. The
//     boundary should never let an empty value through, but defending
//     here keeps the controller usable from tests and ad-hoc callers.
//   - global and window both true: rejected up front as a plain
//     error. tmux would otherwise resolve the contradiction silently
//     in a version-dependent way.
//
// Idempotence: tmux's `set-hook -u` against a hook that was never set
// (or was already cleared by a prior call) succeeds silently — no
// stderr, exit code 0 — so this controller does not need any
// special-case handling for the missing-hook shape. A recovery loop
// that re-issues the same teardown frame sees a clean nil from the
// repeat call.
func (c *Controller) UnsetHook(ctx context.Context, target, hookName string, global, window bool) error {
	if hookName == "" {
		return errors.New("hook name required")
	}
	if global && window {
		return errors.New("global and window are mutually exclusive")
	}
	// Per-session unset path: target is required because tmux's
	// "" → current-session resolution would silently no-op against
	// whatever session the daemon last touched, which would mis-route
	// the teardown against a stale target. Surface this as the
	// session-not-found sentinel so the JSON-RPC layer reports the
	// stable CodeSessionNotFound (-32000) instead of a version-
	// dependent "no current target" stderr.
	if !global && !window && target == "" {
		return fmt.Errorf("hook target required when neither global nor window: %w", errs.ErrSessionNotFound)
	}
	args := []string{"set-hook", "-u"}
	switch {
	case global:
		args = append(args, "-g")
	case window:
		// `-w` clears the hook on the window options table. tmux still
		// honours `-t TARGET` alongside `-w` to scope the clear; without
		// `-t`, tmux uses the current window (which the caller is
		// allowed to lean on if they really want the current-window
		// shape — the per-session "" → current resolution footgun does
		// not apply here because window hooks are cheap to re-apply).
		args = append(args, "-w")
		if target != "" {
			args = append(args, "-t", target)
		}
	default:
		// Per-session unset: -t TARGET is the only argv shape tmux
		// accepts here. -g and -t are mutually exclusive so the
		// global=true branch above already returned without taking
		// this fork; the empty-target check above already rejected
		// the "" → current shape.
		args = append(args, "-t", target)
	}
	args = append(args, hookName)
	if _, err := c.run(ctx, args...); err != nil {
		// run() already maps "can't find session" / "no such session"
		// to errs.ErrSessionNotFound. tmux 3.4 surfaces some
		// missing-target shapes via the underlying window-options
		// machinery instead — "no such window: <target>" — because
		// hooks live in the per-window options table. Fold those
		// (and the "no current target" headless shape) into the same
		// sentinel so callers can errors.Is against ErrSessionNotFound
		// regardless of which exact phrase tmux emitted.
		//
		// Likewise, unsetting against a daemon with no live sessions
		// surfaces as "no current target" / "no server running" /
		// "error connecting"; map those to the same sentinel so a
		// headless `unset_hook` call returns a clean, typed
		// CodeSessionNotFound rather than a generic internal error.
		msg := strings.ToLower(err.Error())
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			(strings.Contains(msg, "no such window") ||
				strings.Contains(msg, "no current target") ||
				strings.Contains(msg, "no server running") ||
				strings.Contains(msg, "error connecting") ||
				strings.Contains(msg, "server exited unexpectedly") ||
				strings.Contains(msg, "no such file or directory")) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

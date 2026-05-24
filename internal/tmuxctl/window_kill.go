package tmuxctl

import (
	"context"
	"errors"
)

// KillWindowResult is the structured outcome of a [Controller.KillWindowReport]
// call. Two flags rather than one because tmux's kill-window has two
// distinct success shapes that callers need to distinguish:
//
//   - The targeted window goes away and its session keeps living
//     (Killed=true, SessionKilled=false) — the common case.
//   - The targeted window was the only window in the session, so tmux
//     reaps the surrounding session as well (Killed=true,
//     SessionKilled=true). Reporting that explicitly lets boundary tools
//     surface the cascade to the agent without forcing it to issue a
//     follow-up session_list.
type KillWindowResult struct {
	// Killed is true on every successful kill-window invocation. Kept as
	// an explicit field (rather than relying on err==nil) so the JSON
	// surface mirrors pane_kill's `{"killed": true}` ack and gives
	// future cases (e.g. soft-kill modes that report Killed=false) a
	// stable home to live in.
	Killed bool
	// SessionKilled is true when the kill collapsed the session because
	// the targeted window was the last one. Detected by re-checking the
	// session's existence after the kill via [Controller.HasSession];
	// tmux itself produces no useful stdout/stderr on that branch, so
	// inspecting the session list afterwards is the most reliable way
	// to tell the cases apart across tmux versions.
	SessionKilled bool
}

// KillWindowReport is a result-returning variant of [Controller.KillWindow]
// designed for the boundary tool that wants to expose tmux's "kill the
// last window also kills the session" cascade as a structured flag
// rather than refusing the call. It targets the same `<session>:<window>`
// the bare KillWindow does, but on success returns a [KillWindowResult]
// describing whether the session itself was reaped.
//
// `window` may be either a window name or a numeric index — tmux
// resolves both forms uniformly via `kill-window -t <session>:<window>`.
//
// A missing session/window surfaces as a wrapped errs.ErrSessionNotFound
// (via the underlying KillWindow → run() chain) so the JSON-RPC
// dispatcher can map it to CodeSessionNotFound the same way every other
// session-bearing tool does. The post-kill HasSession probe is best-
// effort: any error from that re-check is propagated up because we
// cannot honestly tell the caller whether the session survived without
// it, and silently lying about SessionKilled would be worse than
// surfacing the rare list-sessions failure.
func (c *Controller) KillWindowReport(ctx context.Context, session, window string) (KillWindowResult, error) {
	if session == "" {
		return KillWindowResult{}, errors.New("session required")
	}
	if window == "" {
		return KillWindowResult{}, errors.New("window required")
	}
	if err := c.KillWindow(ctx, session, window); err != nil {
		return KillWindowResult{}, err
	}
	// tmux's kill-window emits no useful stdout, and the stderr it would
	// emit when the session got reaped along with the window is not
	// stable across versions ("session not found", "session destroyed",
	// or sometimes nothing at all). Re-querying the session list is the
	// boring-but-reliable signal: if the session still resolves we know
	// only the window was killed; otherwise the cascade fired.
	exists, err := c.HasSession(ctx, session)
	if err != nil {
		return KillWindowResult{}, err
	}
	return KillWindowResult{
		Killed:        true,
		SessionKilled: !exists,
	}, nil
}

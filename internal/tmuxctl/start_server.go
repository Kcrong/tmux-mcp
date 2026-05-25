package tmuxctl

import (
	"context"
	"strings"
)

// StartServer wraps `tmux start-server`. It tells tmux to spawn the
// daemon for this controller's private socket without creating any
// session, which is useful for warming the controller before a flurry
// of CreateSession calls — the first session no longer pays the
// daemon-spawn cost on its critical path.
//
// Idempotent by design: tmux's `start-server` is a no-op when a server
// is already listening on the socket, so callers can issue this at
// startup unconditionally without having to first probe for liveness.
//
// The call is dispatched through [Controller.run] so it picks up the
// same -S socket path and -f config flag every other tmux invocation
// uses; that keeps the warmed daemon byte-identical to the one a later
// CreateSession would have spawned implicitly.
//
// Heavy-parallel CI environments occasionally surface a benign race
// where tmux spawns the daemon, the client process tries to connect,
// and the daemon exits between spawn and accept — typically because
// the parent's exit closed the only thing keeping the daemon's stdin
// alive long enough. tmux reports this as `server exited unexpectedly`
// even though no operator-visible failure is involved. Retrying the
// `start-server` call once produces a fresh daemon and connects to it
// cleanly, so we fold the recovery into the wrapper here. A single
// retry is enough: a genuinely broken environment (binary missing,
// socket dir not writable, …) keeps failing on the second attempt and
// the caller sees the same error it would have seen before.
func (c *Controller) StartServer(ctx context.Context) error {
	_, err := c.run(ctx, "start-server")
	if err != nil && strings.Contains(err.Error(), "server exited unexpectedly") {
		// Retry once. The transient exit only manifests when tmux
		// spawned a daemon and lost it before the client connected —
		// the second call either reuses an already-running daemon
		// (idempotent path) or spawns a fresh one.
		_, err = c.run(ctx, "start-server")
	}
	return err
}

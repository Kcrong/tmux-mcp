package tmuxctl

import "context"

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
func (c *Controller) StartServer(ctx context.Context) error {
	_, err := c.run(ctx, "start-server")
	return err
}

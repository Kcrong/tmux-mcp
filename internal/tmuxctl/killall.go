package tmuxctl

import "context"

// KillAllSessions enumerates and kills every session this controller
// knows about. Returns the names of sessions that were killed (in any
// order), plus the first error encountered (if any). The controller's
// tmux server itself is left running so the next CreateSession does not
// need to re-spawn it.
//
// Best-effort semantics: a single broken session does not strand the
// rest. We attempt to kill every session ListSessions returned and
// only report the first non-nil error to the caller.
func (c *Controller) KillAllSessions(ctx context.Context) ([]string, error) {
	names, err := c.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	killed := make([]string, 0, len(names))
	var firstErr error
	for _, name := range names {
		if kerr := c.KillSession(ctx, name); kerr != nil {
			if firstErr == nil {
				firstErr = kerr
			}
			continue
		}
		killed = append(killed, name)
	}
	return killed, firstErr
}

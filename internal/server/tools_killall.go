package server

import (
	"context"
	"encoding/json"
)

// handleKillAll backs the kill_all_sessions tool. It kills every session
// the controller knows about, forgets the snapshot history for each
// killed name so we don't leak entries, and returns the list of killed
// names plus a count for callers that want a quick summary.
//
// The handler ignores its arguments (the schema declares no fields) and
// is intentionally tolerant of an empty/null arguments value so clients
// can call it without sending an arguments object at all.
//
// When -session-prefix is set the kill scope is constrained to sessions
// whose name carries the prefix; cross-prefix sessions on the same tmux
// server are left running so a co-tenant agent's work is never reaped
// by mistake. The returned `killed` list is also stripped of the prefix
// so the client sees the same logical names it created.
func (t *Tools) handleKillAll(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	if t.SessionPrefix == "" {
		killed, err := t.Ctl.KillAllSessions(ctx)
		// Forget snapshot history for every session we successfully
		// killed, even when KillAllSessions reports a partial error.
		// Holding onto history for a session that no longer exists
		// would leak entries.
		for _, name := range killed {
			t.Snap.Forget(name)
		}
		if err != nil {
			return nil, internalError(err)
		}
		return jsonBlock(map[string]any{
			"killed": killed,
			"count":  len(killed),
		})
	}
	// Prefixed mode: enumerate, filter, kill one-by-one. Doing this in
	// the handler (rather than in tmuxctl.KillAllSessions) keeps the
	// controller surface free of dispatcher concerns and lets us strip
	// the prefix from the response without a second round-trip.
	all, err := t.Ctl.ListSessions(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	killed := make([]string, 0, len(all))
	var firstErr error
	for _, name := range all {
		if !t.hasSessionPrefix(name) {
			continue
		}
		if kerr := t.Ctl.KillSession(ctx, name); kerr != nil {
			if firstErr == nil {
				firstErr = kerr
			}
			continue
		}
		t.Snap.Forget(name)
		killed = append(killed, t.stripSessionPrefix(name))
	}
	if firstErr != nil {
		return nil, internalError(firstErr)
	}
	return jsonBlock(map[string]any{
		"killed": killed,
		"count":  len(killed),
	})
}

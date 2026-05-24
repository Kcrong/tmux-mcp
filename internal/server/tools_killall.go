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
func (t *Tools) handleKillAll(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	killed, err := t.Ctl.KillAllSessions(ctx)
	// Forget snapshot history for every session we successfully killed,
	// even when KillAllSessions reports a partial error. Holding onto
	// history for a session that no longer exists would leak entries.
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

package server

import (
	"context"
	"encoding/json"
)

// startServerToolDefs holds the JSON Schema for the start_server tool.
// It is appended onto the main toolDefs slice from this file's init()
// so the registration site stays close to the handler — the dispatcher
// in tools.go only needs the single name → handler entry.
//
// start_server pre-spawns the controller's tmux daemon without creating
// any session, so an agent that knows it is about to issue a flurry of
// session_create calls can pay the daemon-spawn cost once at warm-up
// instead of on the first session_create's critical path. The call is
// idempotent: when a server is already listening on the controller's
// socket it succeeds without doing anything, so deployment scripts can
// run it unconditionally on every startup.
//
// Mutating in spirit (it spawns a daemon process) — deliberately
// excluded from the read-only allowlist so a -read-only operator can't
// accidentally bring up a daemon they only meant to inspect.
var startServerToolDefs = []map[string]any{
	{
		"name": "start_server",
		"description": "Spawn this controller's tmux daemon via `tmux start-server` without creating " +
			"any session. Pairs well with session_create — agents that pre-warm the daemon " +
			"avoid the spawn cost on the first session_create's critical path. Idempotent: " +
			"when a server is already listening on the controller's socket the call is a " +
			"no-op and returns success. Takes no arguments — pass `{}`.",
		"inputSchema": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register start_server onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, startServerToolDefs...)
}

// startServer drives tmuxctl.Controller.StartServer. The handler ignores
// its arguments (the schema declares no fields) and is intentionally
// tolerant of an empty/null arguments value so clients can call it
// without sending an arguments object at all.
//
// Response is a small JSON ack (`{"started": true}`) so callers that
// chain start_server → session_create can branch on a stable shape
// rather than parse a free-form status string. The ack is identical
// whether the daemon was just spawned or was already running because
// tmux's `start-server` itself does not distinguish the two — and
// surfacing that detail would push every caller to write a
// "warm or already-warm?" branch they do not need.
func (t *Tools) startServer(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	if err := t.Ctl.StartServer(ctx); err != nil {
		return nil, internalError(err)
	}
	return jsonBlock(map[string]any{"started": true})
}

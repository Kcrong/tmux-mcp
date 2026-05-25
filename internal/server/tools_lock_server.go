package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// lockServerToolDefs holds the JSON Schema for the lock_server tool.
// It is appended onto the main toolDefs slice from this file's init()
// so the registration site stays close to the handler — the dispatcher
// in tools.go only needs the single name → handler entry.
//
// lock_server is the simplest of the three lock primitives: tmux's
// `lock-server` (alias `lock`) takes no flags at all. The schema is a
// closed empty-properties object so a typo'd field (e.g. "session" or
// "client", probably borrowed from the lock_session / lock_client
// schemas) fails fast with -32602 instead of being silently ignored —
// which would risk masking a caller bug that meant to target one
// specific client/session and accidentally locked the entire server.
var lockServerToolDefs = []map[string]any{
	{
		"name": "lock_server",
		"description": "Lock the entire tmux server via `tmux lock-server` (alias `lock`). " +
			"tmux iterates every attached client on this controller's private daemon and " +
			"runs the configured `lock-command` (default `lock -np`) against each one. " +
			"Distinct from lock_session (locks every client attached to one named session) " +
			"and lock_client (locks one specific TTY): lock_server covers every screen on " +
			"every session this daemon is hosting in a single call. " +
			"Headless servers with nothing attached are a successful no-op — tmux still " +
			"exits 0 because the iteration over attached clients is empty. Returns a " +
			"small JSON ack `{\"locked\": true}`. Takes no arguments — pass `{}`.",
		"inputSchema": map[string]any{
			"type": "object",
			// Empty properties + additionalProperties:false locks the
			// schema down so any field a caller invents (e.g. "session"
			// borrowed from lock_session, or "client" from lock_client)
			// is rejected with -32602 before tmux is consulted. tmux's
			// own `lock-server` takes no flags, so the closed schema
			// makes the boundary policy match the tmux surface exactly.
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register lock_server onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, lockServerToolDefs...)
}

// lockServer drives tmuxctl.Controller.LockServer and serialises the
// result to the standard
// `{"content":[{"type":"text","text":"<json>"}]}` envelope MCP expects
// from a tools/call. The response shape is a flat object keyed by
// "locked" so a future addition (e.g. a count of clients tmux iterated
// over, if a later version of tmux ever surfaces it on stdout) can
// land alongside without breaking callers that only read the boolean.
//
// Argument handling:
//   - the schema declares no fields and additionalProperties:false; the
//     handler tolerates an empty/null arguments value so clients that
//     omit the arguments object entirely (some MCP clients serialise an
//     empty argument set as a literal `null`) still work.
//
// Error mapping:
//   - no server running → -32000 (CodeSessionNotFound), via the wrapped
//     errs.ErrSessionNotFound the controller emits. Same code every
//     other "named target does not exist" path uses (lock_session,
//     lock_client, list_clients, session_kill, …).
//   - any other tmux failure → -32603 (internal).
//
// This is a MUTATING tool (it changes what every attached client's
// terminal displays — the lock screen replaces the live session view
// across every session on this server), so it is deliberately NOT in
// readOnlyTools — a -read-only deployment must reject it before the
// handler runs.
func (t *Tools) lockServer(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	if err := t.Ctl.LockServer(ctx); err != nil {
		return nil, internalError(fmt.Errorf("lock_server: %w", err))
	}
	return jsonBlock(map[string]any{"locked": true})
}

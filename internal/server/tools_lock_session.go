package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// lockSessionToolDefs holds the JSON Schema for the lock_session tool.
// The block is appended onto the main toolDefs slice from this file's
// init() so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
var lockSessionToolDefs = []map[string]any{
	{
		"name": "lock_session",
		"description": "Lock every client attached to a session via `tmux lock-session -t SESSION`. " +
			"tmux runs the configured `lock-command` (default `lock -np`) on each attached client, " +
			"so the user has to authenticate before resuming work. Running processes inside the " +
			"session are left untouched and the session itself stays valid for follow-up tools — " +
			"only the attached clients see the lock screen. Useful when an agent is handing a " +
			"long-running session back to a human and wants the screen secured. Headless servers " +
			"(the common case for tmux-mcp) have nothing to lock; tmux still exits 0 because the " +
			"loop over attached clients is empty, so the call is safe to make from automation that " +
			"does not know whether anyone is currently attached.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"maxLength":   maxSessionNameLen,
					"description": "Existing session id to lock; len 1-64, [A-Za-z0-9_-].",
				},
			},
			"required": []string{"session"},
			// Lock the schema so a typo'd field (e.g. "sesion", "name")
			// fails fast with -32602 instead of silently behaving like
			// an empty target — tmux would otherwise resolve "" to the
			// current session, which is almost never the intent.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register lock_session onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, lockSessionToolDefs...)
}

// lockSession drives tmuxctl.Controller.LockSession. The handler
// validates the required `session` shape up front so a caller passing
// a malformed value sees CodeInvalidParams (-32602) before any tmux
// command runs. The response is a small JSON ack `{"locked": true}` —
// tmux's lock-session reports nothing of the sort itself, and a
// follow-up list_clients is one call away if the agent wants to
// inspect which clients were affected.
//
// Mutating tool: deliberately excluded from the read-only allowlist in
// readonly.go. lock-session writes to every attached client's terminal
// (the lock screen) and changes the visible state for human users, so
// it does not satisfy the read-only contract even though the
// session's running processes are undisturbed.
func (t *Tools) lockSession(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("lock_session: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if err := t.Ctl.LockSession(ctx, t.resolveSessionRef(args.Session)); err != nil {
		return nil, internalError(fmt.Errorf("lock_session: %w", err))
	}
	return jsonBlock(map[string]any{"locked": true})
}

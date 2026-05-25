package server

import (
	"context"
	"encoding/json"
)

// hasSessionToolDefs holds the JSON Schema for the has_session tool. It
// is appended onto the main toolDefs slice via the package init() in
// this file so the registration site stays close to the handler — and
// the dispatcher in tools.go only needs the single name → handler entry.
//
// The tool answers the very common "does session X exist?" probe an
// agent runs before send_keys / capture / inspect. Compared with
// session_list (which returns every session's name) or session_describe
// (which also issues a display-message for layout fields), has_session
// is strictly the cheapest path: a single tmux IPC and a one-bit answer.
var hasSessionToolDefs = []map[string]any{
	{
		"name": "has_session",
		"description": "Report whether the named session currently exists on this server. Wraps " +
			"tmux's has-session primitive — strictly cheaper than session_list when the " +
			"caller only needs a yes/no answer (e.g. before deciding whether to " +
			"session_create or jump straight to send_keys). Returns {\"exists\": " +
			"true|false}. A missing session is the literal answer the caller asked for, " +
			"NOT an error: only malformed args (-32602) or genuine tmux failures " +
			"(-32603) surface as JSON-RPC errors.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required":             []string{"name"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register has_session onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from a single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, hasSessionToolDefs...)
}

// hasSession drives tmuxctl.Controller.HasSession and returns the
// existence flag verbatim in a {"exists": bool} JSON envelope.
//
// The load-bearing contract: a missing session is the literal answer
// the caller asked for, so we deliberately do NOT translate it to
// errs.ErrSessionNotFound (-32000). Only malformed args
// (invalidParams / -32602) or genuine controller failures
// (internalError / -32603) surface as JSON-RPC errors. This is what
// makes the tool useful — an agent can ask "is X there?" without
// having to catch -32000 just to learn "no".
//
// Under -session-prefix, the caller's logical name is rewritten to the
// actual tmux session name before the lookup, so the tool sees the
// same prefix-isolation rules every other session-bearing tool does.
func (t *Tools) hasSession(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("has_session: %v", err)
	}
	if rerr := validateSessionName(args.Name); rerr != nil {
		return nil, rerr
	}
	exists, err := t.Ctl.HasSession(ctx, t.resolveSessionRef(args.Name))
	if err != nil {
		// HasSession's contract is "(false, nil) for missing; non-nil
		// err only on real failures", so any error here is a genuine
		// tmux/controller problem worth surfacing to the caller.
		return nil, internalError(err)
	}
	return jsonBlock(map[string]any{"exists": exists})
}

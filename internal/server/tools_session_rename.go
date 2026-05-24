package server

import (
	"context"
	"encoding/json"
)

// renameToolDefs holds the JSON Schema for the session_rename tool. The
// block is appended onto the main toolDefs slice from this file's
// init() so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
var renameToolDefs = []map[string]any{
	{
		"name": "session_rename",
		"description": "Rename an existing tmux session via `tmux rename-session -t OLD NEW`. " +
			"Useful when an agent's first label was a placeholder (\"scratch\") and the " +
			"work has settled into a recognisable identity (\"build-3128\"). After the call, " +
			"session_describe / session_list / send_keys / capture must all be addressed by " +
			"the new name; the old name is gone.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"maxLength":   maxSessionNameLen,
					"description": "Existing session name to rename; len 1-64, [A-Za-z0-9_-].",
				},
				"new_name": map[string]any{
					"type":        "string",
					"maxLength":   maxSessionNameLen,
					"description": "New session name; same regex/length policy as `name`.",
				},
			},
			"required": []string{"name", "new_name"},
			// Lock the schema so a typo'd field (e.g. "newname",
			// "rename") fails fast with -32602 instead of silently
			// behaving like an empty rename target.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register session_rename onto the main toolDefs slice. Doing this
	// from init() keeps the new tool's surface entirely contained in
	// this file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, renameToolDefs...)
}

// sessionRename drives tmuxctl.Controller.RenameSession. Validates both
// names against the standard session-name policy before any tmux call
// is made, then maps tmux's typed sentinels onto the wire codes the rest
// of the dispatcher uses (ErrSessionNotFound → -32000, ErrSessionExists
// → -32004 via internalError + errs.CodeOf).
//
// The response is the renamed pair — useful for clients that fire-and-
// forget the call but still want to confirm the new identity in the
// audit trail without a separate session_list round-trip.
func (t *Tools) sessionRename(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Name    string `json:"name"`
		NewName string `json:"new_name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("session_rename: %v", err)
	}
	if rerr := validateSessionName(args.Name); rerr != nil {
		return nil, rerr
	}
	// Reuse validateSessionName for the destination so both names share
	// the same regex/length contract — divergence here would let one
	// arg sneak past the rules the other enforces.
	if args.NewName == "" {
		return nil, invalidParams("new_name required")
	}
	if rerr := validateSessionName(args.NewName); rerr != nil {
		// Re-shape the error message so the offending field is obvious
		// to the caller. The underlying invalidParams from
		// validateSessionName mentions "session name", which would be
		// ambiguous when both args use the same validator.
		return nil, invalidParams("new_name: %s", rerr.Message)
	}
	if args.Name == args.NewName {
		// tmux refuses this with "duplicate session" too, but a
		// dedicated -32602 message ("nothing to do") is friendlier and
		// keeps the CodeSessionExists semantics tied to a real
		// collision with a different session.
		return nil, invalidParams("name and new_name are equal; nothing to rename")
	}
	if err := t.Ctl.RenameSession(ctx, args.Name, args.NewName); err != nil {
		return nil, internalError(err)
	}
	// Echo the rename back so audit logs / clients have the pre/post
	// identity without a follow-up session_list. Snapshot history is
	// keyed by name; we don't migrate it here because the rename does
	// not preserve the pane content the snapshot store cached against
	// the old key — the next capture against the new name will seed a
	// fresh entry naturally.
	return jsonBlock(map[string]any{
		"old_name": args.Name,
		"new_name": args.NewName,
	})
}

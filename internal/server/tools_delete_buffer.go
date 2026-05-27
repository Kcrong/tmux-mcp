package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// deleteBufferToolDefs holds the JSON Schema for the delete_buffer
// tool. It is appended onto the main toolDefs slice via this file's
// init() so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
//
// Paste buffers live on the tmux server (not on a session), so this
// tool deliberately is not session-scoped: there is no `session`
// field in the schema and SessionPrefix does not apply.
//
// `name` is required by design: tmux's bare `delete-buffer` (no -b)
// drops the most-recently-added buffer, but exposing that
// "delete the last thing you stored" path through a programmatic
// agent invites accidental destruction of buffers another caller
// just minted. Forcing the name keeps the operation deterministic
// from the caller's point of view.
var deleteBufferToolDefs = []map[string]any{
	{
		"name": "delete_buffer",
		"description": "Drop a single named tmux paste buffer via `tmux delete-buffer -b NAME`. " +
			"Useful for an agent that stashed a snippet via `set_buffer` and wants to release " +
			"the storage once the value has been consumed — buffers persist on the tmux server " +
			"until explicitly deleted (or until tmux's `buffer-limit` rotates them out), so a " +
			"long-running agent that writes many buffers should clean up the ones it no longer " +
			"needs. Returns `{\"deleted\": true, \"name\": \"<name>\"}` on success. The named " +
			"buffer must exist; deleting a missing buffer surfaces -32000 (`errs.ErrSessionNotFound`) " +
			"so callers can branch on the same stable wire code `show_buffer` already uses for the " +
			"same conceptual outcome.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Buffer name to drop. Required; len 1-128, regex `^[A-Za-z0-9_-]+$`.",
				},
			},
			"required":             []string{"name"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register delete_buffer onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from a single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, deleteBufferToolDefs...)
}

// deleteBuffer drives tmuxctl.Controller.DeleteBuffer. The handler
// validates `name` up front against the same regex/length policy
// used elsewhere on the buffer surface so a stray quote or shell
// metachar cannot slip through to tmux's argv. A genuinely missing
// buffer is surfaced as CodeSessionNotFound (-32000) so MCP clients
// can branch on the same stable wire code show_buffer already uses
// for the same conceptual outcome.
func (t *Tools) deleteBuffer(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Name string `json:"name"`
	}
	// Allow an explicit `null` / empty body so a tools/call frame with
	// `arguments: {}` still surfaces the required-field validation
	// below rather than choking on the unmarshal.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("delete_buffer: %v", err)
		}
	}
	// `name` is required — tmux's bare `delete-buffer` (no -b) drops
	// the most-recently-added buffer, and exposing that path through
	// the MCP surface invites accidental destruction. Force the
	// caller to be explicit.
	if args.Name == "" {
		return nil, invalidParams("name: required")
	}
	// Reuse validateBufferName from tools_buffers.go; the policy is
	// identical to show_buffer's optional-name validator (the only
	// difference is that delete_buffer requires the name, which the
	// emptiness check above already enforces).
	if rerr := validateBufferName(args.Name); rerr != nil {
		return nil, invalidParams("delete_buffer: %s", rerr.Message)
	}
	if err := t.Ctl.DeleteBuffer(ctx, args.Name); err != nil {
		// internalError() routes ErrSessionNotFound (wrapped at the
		// controller boundary when tmux reports "no buffer NAME") to
		// CodeSessionNotFound (-32000) via errs.CodeOf, so MCP clients
		// can branch on the same stable wire code show_buffer already
		// uses for the same conceptual outcome. Anything else falls
		// back to CodeInternal (-32603).
		return nil, internalError(fmt.Errorf("delete_buffer: %w", err))
	}
	return jsonBlock(map[string]any{
		"deleted": true,
		"name":    args.Name,
	})
}

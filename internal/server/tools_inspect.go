package server

import (
	"context"
	"encoding/json"
)

// inspectToolDefs holds the JSON Schema for the session_inspect tool.
// It is appended onto the main toolDefs slice via the package init() in
// this file so the registration site stays close to the handler — and
// the dispatcher in tools.go only needs the single name → handler entry.
var inspectToolDefs = []map[string]any{
	{
		"name": "session_inspect",
		"description": "Return process-level metadata for a session's active pane: foreground PID, " +
			"current working directory, and command name (e.g. \"bash\", \"vim\"). Useful for " +
			"debugging a stuck shell or for routing follow-up commands based on which tool the " +
			"user is currently running. Distinct from session_describe, which reports " +
			"session-level layout (window/pane counts, geometry, creation time). " +
			"Environment variables are intentionally NOT exposed because they routinely carry " +
			"tokens and other secrets.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{"type": "string"},
			},
			"required": []string{"session"},
		},
	},
}

func init() {
	// Register session_inspect onto the main toolDefs slice. Doing
	// this from init() keeps the new tool surface entirely contained
	// in this file (apart from a single dispatcher case in tools.go)
	// and avoids touching the shared toolDefs literal that other PRs
	// are editing.
	toolDefs = append(toolDefs, inspectToolDefs...)
}

// sessionInspect drives tmuxctl.Controller.InspectSession and
// serialises the result to the standard
// `{"content":[{"type":"text","text":"<json>"}]}` envelope MCP expects
// for tools/call. The output shape is intentionally a flat object so
// future additions (e.g. a "started_at" field) do not break callers
// that read the fields they care about.
//
// Unknown session names are surfaced via the wrapped
// errs.ErrSessionNotFound sentinel, which the JSON-RPC layer maps to
// CodeSessionNotFound (-32000).
func (t *Tools) sessionInspect(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("session_inspect: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	info, err := t.Ctl.InspectSession(ctx, t.resolveSessionRef(args.Session))
	if err != nil {
		return nil, internalError(err)
	}
	return jsonBlock(map[string]any{
		"name":    args.Session,
		"pid":     info.PID,
		"cwd":     info.Cwd,
		"command": info.Command,
	})
}

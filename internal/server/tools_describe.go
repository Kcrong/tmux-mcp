package server

import (
	"context"
	"encoding/json"
	"time"
)

// describeToolDefs holds the JSON Schema for the session_describe tool.
// It is appended onto the main toolDefs slice via the package init() in
// this file so the registration site stays close to the handler — and
// the dispatcher in tools.go only needs the single name → handler entry.
var describeToolDefs = []map[string]any{
	{
		"name": "session_describe",
		"description": "Return structured metadata for a single session: window count, total " +
			"pane count, current width/height (cols × rows), and the creation " +
			"timestamp as RFC3339. Useful when an agent needs to confirm a session " +
			"layout or correlate logs by creation time.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required": []string{"name"},
		},
	},
}

func init() {
	// Register session_describe onto the main toolDefs slice. Doing
	// this from init() keeps the new tool surface entirely contained
	// in this file (apart from a single dispatcher case in tools.go)
	// and avoids touching the shared toolDefs literal that other PRs
	// are editing.
	toolDefs = append(toolDefs, describeToolDefs...)
}

// sessionDescribe drives tmuxctl.Controller.DescribeSession and
// serialises the result to the standard `{"content":[{"type":"text",
// "text":"<json>"}]}` envelope MCP expects for tools/call. The output
// shape is intentionally a flat object so future additions (e.g. a
// "history_limit" or "pid" field) do not break callers that read the
// fields they care about.
//
// Unknown session names are surfaced via the wrapped
// errs.ErrSessionNotFound sentinel, which the JSON-RPC layer maps to
// CodeSessionNotFound (-32000).
func (t *Tools) sessionDescribe(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("session_describe: %v", err)
	}
	if rerr := validateSessionName(args.Name); rerr != nil {
		return nil, rerr
	}
	info, err := t.Ctl.DescribeSession(ctx, args.Name)
	if err != nil {
		return nil, internalError(err)
	}
	return jsonBlock(map[string]any{
		"name":       info.Name,
		"windows":    info.Windows,
		"panes":      info.Panes,
		"width":      info.Width,
		"height":     info.Height,
		"created_at": info.CreatedAt.UTC().Format(time.RFC3339),
	})
}

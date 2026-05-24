package server

import (
	"context"
	"encoding/json"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// panesToolDefs holds the JSON Schemas for the multi-pane tools. They
// are appended onto the main toolDefs slice via the package init() in
// this file so the registration site stays close to the handlers — and
// the dispatcher in tools.go only needs the two name → handler entries.
var panesToolDefs = []map[string]any{
	{
		"name": "list_panes",
		"description": "List panes visible to this server. Pass session to scope the listing to a single tmux " +
			"session; omit it to enumerate every pane on the server (-a). Each entry includes the " +
			"\"session:window\" pair plus the pane index, so callers can build a \"session:window.pane\" " +
			"target for pane_select / send_keys / capture.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{"type": "string"},
			},
		},
	},
	{
		"name": "pane_select",
		"description": "Make target the active pane of its window. target is a tmux \"session:window.pane\" " +
			"string (e.g. \"demo:0.1\"). Subsequent send_keys / capture calls that name the session " +
			"will then act on the newly selected pane.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{"type": "string"},
			},
			"required": []string{"target"},
		},
	},
}

func init() {
	// Register the multi-pane tools onto the main toolDefs slice. Doing
	// this from init() keeps the new tool surface entirely contained in
	// this file (apart from the dispatcher cases in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, panesToolDefs...)
}

// listPanes drives tmuxctl.Controller.ListPanes and serialises the
// result to the {content: [{type: text, text: "<json>"}]} envelope MCP
// expects from a tools/call. The shape is intentionally a flat object
// keyed by "panes" so a future addition (e.g. a "scope" or
// "active_only" filter) does not break callers that iterate the list.
func (t *Tools) listPanes(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
	}
	// json.Unmarshal on an empty payload is fine — both the schema and
	// the dispatcher allow `arguments: {}` here, and the zero value of
	// args.Session means "list every pane on the server".
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("list_panes: %v", err)
		}
	}
	panes, err := t.Ctl.ListPanes(ctx, args.Session)
	if err != nil {
		return nil, internalError(err)
	}
	out := make([]map[string]any, 0, len(panes))
	for _, p := range panes {
		out = append(out, paneToMap(p))
	}
	return jsonBlock(map[string]any{"panes": out})
}

// paneToMap turns a tmuxctl.Pane into the JSON-friendly map the tool
// returns. The keys mirror the Pane field names (snake_case) so
// downstream agents can index into the response without an extra
// translation step.
func paneToMap(p tmuxctl.Pane) map[string]any {
	return map[string]any{
		"id":          p.ID,
		"title":       p.Title,
		"session_win": p.SessionWin,
		"index":       p.Index,
		"active":      p.Active,
		"width":       p.Width,
		"height":      p.Height,
	}
}

// paneSelect drives tmuxctl.Controller.SelectPane. The handler
// validates that target is non-empty up front so the JSON-RPC error
// shape stays consistent with the other params-validation paths.
func (t *Tools) paneSelect(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("pane_select: %v", err)
	}
	if args.Target == "" {
		return nil, invalidParams("pane_select: target required")
	}
	if err := t.Ctl.SelectPane(ctx, args.Target); err != nil {
		return nil, internalError(err)
	}
	return textBlock("ok"), nil
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// paneBreakToolDefs holds the JSON Schema for the pane_break tool. It
// is appended onto the main toolDefs slice from this file's init() so
// the registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
var paneBreakToolDefs = []map[string]any{
	{
		"name": "pane_break",
		"description": "Detach a pane from its window into a brand-new window via " +
			"`tmux break-pane -P -F \"#{window_id}\" -s <target>`. tmux moves the targeted pane " +
			"out of its current window (which keeps its remaining panes) and re-homes it as the " +
			"sole pane of a freshly-created window on the same session. `target` accepts any tmux " +
			"pane-target form (\"session\", \"session:window\", or \"session:window.pane\"). " +
			"Returns a JSON ack `{\"window\": \"@7\"}` carrying the new window's tmux " +
			"`#{window_id}` — stable for the lifetime of the window and ready to hand to " +
			"window_select / list_panes / send_keys.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Pane target (\"session\", \"session:window\", or \"session:window.pane\").",
				},
			},
			"required":             []string{"target"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register pane_break onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, paneBreakToolDefs...)
}

// paneBreak drives tmuxctl.Controller.BreakPane. The handler does the
// usual up-front validation: target must be non-empty and pass the
// pane-target regex so a caller passing a malformed value sees
// CodeInvalidParams (-32602) before any tmux command runs. A missing
// pane surfaces as CodeSessionNotFound (-32000) via internalError →
// errs.CodeOf, mirroring pane_swap / pane_kill / pane_resize. The
// response is a JSON ack `{"window": "<#{window_id}>"}` carrying the
// new window's stable tmux id, ready for follow-up window_select /
// list_panes / send_keys.
func (t *Tools) paneBreak(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("pane_break: %v", err)
	}
	if args.Target == "" {
		return nil, invalidParams("pane_break: target required")
	}
	if rerr := validatePaneTarget(args.Target); rerr != nil {
		return nil, invalidParams("pane_break: %s", rerr.Message)
	}
	window, err := t.Ctl.BreakPane(ctx, args.Target)
	if err != nil {
		return nil, internalError(fmt.Errorf("pane_break: %w", err))
	}
	return jsonBlock(map[string]any{"window": window})
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// clearHistoryToolDefs holds the JSON Schema for the clear_history
// tool. It is appended onto the main toolDefs slice from this file's
// init() so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
var clearHistoryToolDefs = []map[string]any{
	{
		"name": "clear_history",
		"description": "Drop the scrollback buffer of a pane via `tmux clear-history -t <target>`. Useful " +
			"when a long-running interactive command (build watcher, log tail) has accumulated " +
			"megabytes of scrollback that bloats subsequent `capture` (mode=scrollback) payloads " +
			"and snapshot diffs. Only the scrollback is dropped — the visible region is left " +
			"untouched, the running process is undisturbed, and the pane id stays valid across " +
			"the call. `target` accepts any tmux pane-target form (\"session\", " +
			"\"session:window\", or \"session:window.pane\"). Returns a small JSON ack " +
			"`{\"cleared\": true}` on success.",
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
	// Register clear_history onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, clearHistoryToolDefs...)
}

// clearHistory drives tmuxctl.Controller.ClearHistory. The handler
// validates the required `target` shape up front so a caller passing a
// malformed value sees CodeInvalidParams (-32602) before any tmux
// command runs. The response is a small JSON ack `{"cleared": true}`;
// the boundary deliberately does not echo the cleared line count
// because tmux clear-history reports nothing of the sort and a
// follow-up capture is one call away if the agent wants confirmation.
func (t *Tools) clearHistory(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("clear_history: %v", err)
	}
	if args.Target == "" {
		return nil, invalidParams("clear_history: target required")
	}
	if rerr := validatePaneTarget(args.Target); rerr != nil {
		return nil, invalidParams("clear_history: %s", rerr.Message)
	}
	if err := t.Ctl.ClearHistory(ctx, t.resolvePaneTarget(args.Target)); err != nil {
		return nil, internalError(fmt.Errorf("clear_history: %w", err))
	}
	return jsonBlock(map[string]any{"cleared": true})
}

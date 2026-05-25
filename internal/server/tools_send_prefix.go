package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// sendPrefixToolDefs holds the JSON Schema for the send_prefix tool.
// It is appended onto the main toolDefs slice from this file's init()
// so the registration site stays close to the handler — the dispatcher
// in tools.go only needs the single name → handler entry.
var sendPrefixToolDefs = []map[string]any{
	{
		"name": "send_prefix",
		"description": "Deliver tmux's configured prefix key (default C-b, or C-a / whatever the " +
			"server has bound) to a target pane via `tmux send-prefix [-2] -t <target>`. " +
			"Useful when an inner TUI (vim, htop, weechat, …) running inside the pane has " +
			"captured the prefix chord for its own purposes and an agent needs to forward " +
			"the literal prefix keystroke through to that inner program. Set `secondary` " +
			"to true to deliver the secondary prefix (`-2`, configured via `prefix2`); " +
			"defaults to false (primary prefix). `target` accepts any tmux pane-target " +
			"form (\"session\", \"session:window\", or \"session:window.pane\").",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Pane target (\"session\", \"session:window\", or \"session:window.pane\").",
				},
				"secondary": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, send the secondary prefix (-2) instead of the primary one.",
				},
			},
			"required":             []string{"target"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register send_prefix onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, sendPrefixToolDefs...)
}

// sendPrefix drives tmuxctl.Controller.SendPrefix. The handler
// validates the required `target` shape up front so a caller passing a
// malformed value sees CodeInvalidParams (-32602) before any tmux
// command runs. The response is the standard "ok" status block; the
// boundary deliberately does not echo the configured prefix because
// tmux's own `display-message #{prefix}` is one call away if the agent
// wants to confirm what was actually delivered.
func (t *Tools) sendPrefix(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target    string `json:"target"`
		Secondary bool   `json:"secondary"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("send_prefix: %v", err)
	}
	if args.Target == "" {
		return nil, invalidParams("send_prefix: target required")
	}
	if rerr := validatePaneTarget(args.Target); rerr != nil {
		return nil, invalidParams("send_prefix: %s", rerr.Message)
	}
	if err := t.Ctl.SendPrefix(ctx, t.resolvePaneTarget(args.Target), args.Secondary); err != nil {
		return nil, internalError(fmt.Errorf("send_prefix: %w", err))
	}
	return textBlock("ok"), nil
}

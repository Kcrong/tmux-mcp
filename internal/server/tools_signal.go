package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// signalToolDefs holds the JSON Schema for the send_signal tool. It is
// appended onto the main toolDefs slice from this file's init() so the
// registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
var signalToolDefs = []map[string]any{
	{
		"name": "send_signal",
		"description": "Deliver a POSIX signal (TERM, HUP, INT, QUIT, USR1, USR2, KILL) to the PID of " +
			"the session's currently active pane. More precise than send_keys \"C-c\" because the " +
			"signal targets the foreground program directly — it works even when the pane has stolen " +
			"the keyboard (raw-mode TUIs, daemons that swallow Ctrl-C). Anything outside the whitelist " +
			"is rejected as invalid params before tmux is consulted.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "Session name; len 1-64, [A-Za-z0-9_-].",
				},
				"signal": map[string]any{
					"type":        "string",
					"enum":        toAnySlice(tmuxctl.SignalNames()),
					"description": "POSIX signal name (without the SIG prefix).",
				},
			},
			"required": []string{"session", "signal"},
		},
	},
}

// toAnySlice is a tiny helper that copies a []string into the []any
// form the JSON-Schema enum field expects. Hand-rolling this keeps the
// registration site free of conversion noise and avoids depending on a
// generics helper that doesn't exist anywhere else in the codebase.
func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func init() {
	// Register send_signal onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, signalToolDefs...)
}

// sendSignal drives tmuxctl.Controller.SendSignal. The handler does
// the same up-front validation as the rest of the tool surface
// (session name regex / length, signal whitelist) so callers get a
// CodeInvalidParams response before any tmux command runs.
func (t *Tools) sendSignal(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
		Signal  string `json:"signal"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("send_signal: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if args.Signal == "" {
		return nil, invalidParams("send_signal: signal required (one of %v)", tmuxctl.SignalNames())
	}
	if !isWhitelistedSignal(args.Signal) {
		return nil, invalidParams(
			"send_signal: signal %q not in whitelist %v",
			args.Signal, tmuxctl.SignalNames(),
		)
	}
	if err := t.Ctl.SendSignal(ctx, args.Session, args.Signal); err != nil {
		return nil, internalError(fmt.Errorf("send_signal: %w", err))
	}
	return textBlock("ok"), nil
}

// isWhitelistedSignal mirrors tmuxctl.resolveSignal but lives in the
// server package so the JSON-RPC layer can return CodeInvalidParams
// instead of waiting for the controller to return a generic error
// (which would map to CodeInternal). The two whitelists are kept in
// sync via SignalNames().
func isWhitelistedSignal(name string) bool {
	for _, n := range tmuxctl.SignalNames() {
		if n == name {
			return true
		}
	}
	return false
}

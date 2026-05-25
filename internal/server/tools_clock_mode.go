package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// maxClockModeTargetLen caps the optional `target` string. tmux pane
// targets in practice are short — "session:window.pane" rarely
// exceeds 40-50 bytes — but we leave headroom to 256 so a deployment
// using a long -session-prefix can still pass an unprefixed-looking
// target and have resolvePaneTarget glue the prefix on without
// hitting this guard. Anything past 256 is almost certainly a typo or
// hostile caller; rejecting it up-front keeps the JSON-RPC frame size
// predictable and matches the spirit of the bounds applied to other
// optional pane-target arguments.
const maxClockModeTargetLen = 256

// clockModeToolDefs holds the JSON Schema for the clock_mode tool. It
// is appended onto the main toolDefs slice from this file's init() so
// the registration site stays close to the handler — the dispatcher
// in tools.go only needs the single name → handler entry.
var clockModeToolDefs = []map[string]any{
	{
		"name": "clock_mode",
		"description": "Put a pane into tmux's built-in clock-mode screensaver via " +
			"`tmux clock-mode [-t <target>]`. The targeted pane (or the current " +
			"one when `target` is omitted) renders a large digital clock until the " +
			"next key arrives — the running process keeps running underneath, only " +
			"the visual takeover is added on top. Useful for \"parking\" a pane " +
			"visually (demo recording, status board, idle indicator) without " +
			"typing keys into the running program. `target` accepts any tmux " +
			"pane-target form (\"session\", \"session:window\", or " +
			"\"session:window.pane\", or a `%N` pane id). Returns a small JSON " +
			"ack `{\"clock_mode\": true}` on success.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":      "string",
					"maxLength": maxClockModeTargetLen,
					"description": "Optional pane target (\"session\", \"session:window\", " +
						"\"session:window.pane\", or a `%N` pane id). Omit to target the " +
						"server's current pane.",
				},
			},
			// clock_mode's surface is locked to the single optional
			// `target` field today. An unknown property is far more
			// likely a typo than a future capability we forgot to
			// advertise, so reject it up-front rather than silently
			// ignore it.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register clock_mode onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from the single dispatcher case in tools.go)
	// and avoids touching the shared toolDefs literal that other PRs
	// are editing.
	toolDefs = append(toolDefs, clockModeToolDefs...)
}

// clockMode drives tmuxctl.Controller.ClockMode. The handler validates
// the optional `target` shape up front so a caller passing a
// malformed value sees CodeInvalidParams (-32602) before any tmux
// command runs. The response is a small JSON ack
// `{"clock_mode": true}` — we deliberately do not echo the resolved
// target because tmux clock-mode is fire-and-forget (no return value)
// and a follow-up `display_message` is one call away if the agent
// wants to confirm `pane_mode == "clock-mode"`.
//
// Empty / omitted `target` is permitted: the controller simply skips
// the `-t` argument and tmux targets the server's current pane.
// Non-empty values must satisfy the same conservative regex/length
// policy applied across the rest of the surface so a stray quote or
// shell metachar cannot slip through to tmux.
//
// Unknown / missing targets surface via the wrapped
// errs.ErrSessionNotFound sentinel, which the dispatcher maps to
// CodeSessionNotFound (-32000). A headless controller (no tmux server
// running yet) is treated the same way — with no server there is no
// pane to enter clock-mode on, so the caller sees a clean "missing"
// error rather than a generic internal failure.
func (t *Tools) clockMode(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target string `json:"target"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}` here, and the zero value of args.Target is the
	// "no target" branch the controller expects.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("clock_mode: %v", err)
		}
	}
	target := args.Target
	if target != "" {
		if len(target) > maxClockModeTargetLen {
			return nil, invalidParams("clock_mode: target length %d exceeds %d",
				len(target), maxClockModeTargetLen)
		}
		if rerr := validatePaneTarget(target); rerr != nil {
			return nil, invalidParams("clock_mode: %s", rerr.Message)
		}
		target = t.resolvePaneTarget(target)
	}
	if err := t.Ctl.ClockMode(ctx, target); err != nil {
		return nil, internalError(fmt.Errorf("clock_mode: %w", err))
	}
	return jsonBlock(map[string]any{"clock_mode": true})
}

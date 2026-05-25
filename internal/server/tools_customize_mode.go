package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// maxCustomizeModeStringLen caps each free-form string field
// (target/format/filter) on customize_mode at 256 bytes. tmux happily
// accepts much longer values for -F / -f (the format DSL is
// recursive), but a realistic call rarely exceeds a few dozen bytes —
// anything past 256 is almost certainly a typo, a hostile caller, or a
// caller that meant to pass a different argument. Bounding here keeps
// the JSON-RPC frame size predictable and matches the conservative
// shape every other boundary tool applies.
const maxCustomizeModeStringLen = 256

// customizeModeToolDefs holds the JSON Schema for the customize_mode
// tool. It is appended onto the main toolDefs slice from this file's
// init() so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
var customizeModeToolDefs = []map[string]any{
	{
		"name": "customize_mode",
		"description": "Open the interactive options/key-bindings editor in a target pane via " +
			"`tmux customize-mode [-N] [-Z] [-t TARGET] [-F FORMAT] [-f FILTER]`. tmux's " +
			"customize-mode is the same UI exposed from `:customize-mode` on the command " +
			"line — a row-oriented browser over server, session, window, and pane options " +
			"(plus key bindings) that the user can tweak in place. Useful for an LLM agent " +
			"that wants to drive the editor from a tool call without knowing the option's " +
			"name in advance. Every argument is optional: omit `target` to land on the " +
			"server's current/active pane; pass `no_close=true` to keep the editor open " +
			"after each commit (`-N`), `zoom=true` to zoom the target pane while the editor " +
			"is up (`-Z`), `format` for a custom row format string (`-F FORMAT`), or " +
			"`filter` for a predicate that hides non-matching rows (`-f FILTER`). Returns a " +
			"small JSON ack `{\"opened\": true}` once tmux confirms the pane is in mode.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"maxLength":   maxCustomizeModeStringLen,
					"description": "Optional pane target (\"session\", \"session:window\", or \"session:window.pane\", or a tmux `%N` pane id). Omit to land on the active pane.",
				},
				"format": map[string]any{
					"type":        "string",
					"maxLength":   maxCustomizeModeStringLen,
					"description": "Optional tmux format string (`-F FORMAT`) controlling how each row in the editor is rendered. Must not contain newlines.",
				},
				"filter": map[string]any{
					"type":        "string",
					"maxLength":   maxCustomizeModeStringLen,
					"description": "Optional tmux filter predicate (`-f FILTER`) hiding rows for which the expression evaluates false. Must not contain newlines.",
				},
				"no_close": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "Pass `-N` so tmux keeps the editor open after each commit (default: editor closes on selection).",
				},
				"zoom": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "Pass `-Z` so tmux zooms the target pane while the editor is up.",
				},
			},
			// customize_mode's surface is locked to (target, format,
			// filter, no_close, zoom) today; an unknown field is far more
			// likely a typo than a future capability we forgot to
			// advertise, so reject it up front rather than silently
			// ignore it.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register customize_mode onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, customizeModeToolDefs...)
}

// customizeMode drives tmuxctl.Controller.CustomizeMode. The handler
// validates every optional string up front so a caller passing a
// malformed value sees CodeInvalidParams (-32602) before any tmux
// command runs. The response is a small JSON ack `{"opened": true}`;
// the boundary deliberately does not echo the editor contents back,
// since the editor is interactive and a follow-up `display_message`
// (e.g. `#{?pane_in_mode,1,0}`) is the canonical way to confirm the
// pane is now in mode.
func (t *Tools) customizeMode(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target  string `json:"target"`
		Format  string `json:"format"`
		Filter  string `json:"filter"`
		NoClose bool   `json:"no_close"`
		Zoom    bool   `json:"zoom"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}` here, and every field is optional. Mirrors
	// choose_tree's no-args path so a caller can drive the editor
	// against the active pane with the tightest possible call.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("customize_mode: %v", err)
		}
	}

	// target: empty is allowed (resolve to the active pane); a non-empty
	// value must pass the conservative pane-target regex so a stray
	// quote / shell metachar cannot slip through to tmux. We rely on
	// validatePaneTarget for the regex/length policy and only add the
	// 256-byte cap on top — the validator's existing 128-byte ceiling
	// (maxSessionNameLen*2) is already tighter, but the schema's
	// maxLength keeps the contract consistent across format / filter.
	if args.Target != "" {
		if len(args.Target) > maxCustomizeModeStringLen {
			return nil, invalidParams("customize_mode: target length %d exceeds %d",
				len(args.Target), maxCustomizeModeStringLen)
		}
		if rerr := validatePaneTarget(args.Target); rerr != nil {
			return nil, invalidParams("customize_mode: %s", rerr.Message)
		}
	}

	// format / filter: optional. When supplied we apply the same
	// "no newlines, length-bounded" policy display_message uses for its
	// format DSL — multi-line values would either be silently joined
	// by tmux (changing the meaning of the call) or would split the
	// JSON-RPC frame budget.
	if args.Format != "" {
		if rerr := validateCustomizeModeString("format", args.Format); rerr != nil {
			return nil, rerr
		}
	}
	if args.Filter != "" {
		if rerr := validateCustomizeModeString("filter", args.Filter); rerr != nil {
			return nil, rerr
		}
	}

	if err := t.Ctl.CustomizeMode(ctx, t.resolvePaneTarget(args.Target), args.Format, args.Filter, args.NoClose, args.Zoom); err != nil {
		return nil, internalError(fmt.Errorf("customize_mode: %w", err))
	}
	return jsonBlock(map[string]any{"opened": true})
}

// validateCustomizeModeString applies the shared "non-empty, no
// newlines, length-bounded" check to the optional format / filter
// arguments. Returns nil for the happy path or a typed -32602 rpc
// error otherwise; the field name is interpolated into the message so
// the caller sees which argument was wrong without having to guess
// from the string contents.
func validateCustomizeModeString(name, value string) *rpcError {
	if strings.ContainsAny(value, "\n\r") {
		return invalidParams("customize_mode: %s must not contain newlines", name)
	}
	if len(value) > maxCustomizeModeStringLen {
		return invalidParams("customize_mode: %s length %d exceeds %d",
			name, len(value), maxCustomizeModeStringLen)
	}
	return nil
}

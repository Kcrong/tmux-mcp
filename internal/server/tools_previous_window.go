package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// previousWindowToolDefs holds the JSON Schema for the previous_window
// tool. It is appended onto the main toolDefs slice from this file's
// init() so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
//
// previous_window wraps `tmux previous-window -t <target> [-a]`: it
// flips the targeted session's active window pointer one slot
// backward, wrapping from index 0 to the highest-numbered window.
// Sibling of next_window — the two tools are deliberately symmetric
// so an agent that drives one does not need to relearn the schema for
// the other.
//
// `target` names the session (tmux's `-t` here is a session target,
// not a window target) and reuses the standard session-name policy
// every other tool applies. `with_alert` mirrors tmux's `-a` flag for
// the rare "step to the previous *alert-flagged* window" case; the
// default of false keeps the common path identical to a bare
// previous-window call.
var previousWindowToolDefs = []map[string]any{
	{
		"name": "previous_window",
		"description": "Move the targeted session's active window pointer one slot backward via " +
			"`tmux previous-window -t <target>`. tmux wraps from index 0 to the highest-numbered " +
			"window so a session sitting on its first window does not refuse the call — it lands " +
			"on the last one instead. `with_alert` (default false) maps to tmux's `-a` flag: when " +
			"true, tmux skips windows that are not alert-flagged and lands on the previous one " +
			"that is. Sibling of next_window; the two tools are deliberately symmetric so an " +
			"agent that drives one does not need to relearn the schema for the other.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Existing session name; len 1-64, [A-Za-z0-9_-].",
				},
				"with_alert": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, step to the previous alert-flagged window only (`-a`).",
				},
			},
			"required":             []string{"target"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register previous_window onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing in parallel.
	toolDefs = append(toolDefs, previousWindowToolDefs...)
}

// previousWindow drives tmuxctl.Controller.PreviousWindow. Validates
// the session reference up front so a malformed `target` fails fast
// with -32602 before any tmux command runs. with_alert is parsed as a
// *bool so the schema's documented default of false is applied
// identically whether the field was missing, null, or explicitly
// false. The successful response is a small JSON ack `{"moved":
// true}` — tmux's previous-window itself produces no useful stdout,
// and a chained list_windows is one call away if the caller wants to
// confirm which slot the active flag landed on.
//
// A missing session surfaces as CodeSessionNotFound (-32000) via
// internalError → errs.CodeOf, mirroring window_select / swap_window.
func (t *Tools) previousWindow(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target    string `json:"target"`
		WithAlert *bool  `json:"with_alert"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("previous_window: %v", err)
	}
	if rerr := validateSessionRef(args.Target); rerr != nil {
		// Re-label the error so the caller sees the field name they
		// actually used (`target`) rather than the generic "session"
		// the helper prints. Keeps CodeInvalidParams messages
		// self-explanatory under tools/call.
		return nil, invalidParams("target: %s", rerr.Message)
	}
	withAlert := false
	if args.WithAlert != nil {
		withAlert = *args.WithAlert
	}
	if err := t.Ctl.PreviousWindow(ctx, t.resolveSessionRef(args.Target), withAlert); err != nil {
		return nil, internalError(fmt.Errorf("previous_window: %w", err))
	}
	return jsonBlock(map[string]any{"moved": true})
}

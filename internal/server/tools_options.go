package server

import (
	"context"
	"encoding/json"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// optionsToolDefs holds the JSON Schema for the show_options tool. It
// is appended onto the main toolDefs slice via the package init() in
// this file so the registration site stays close to the handler — and
// the dispatcher in tools.go only needs the single name → handler
// entry.
var optionsToolDefs = []map[string]any{
	{
		"name": "show_options",
		"description": "Return the resolved tmux option set at a given scope. " +
			"scope=server reports server-wide options (`tmux show-options -s`). " +
			"scope=session reports per-session options for the named session " +
			"(`tmux show-options -t SESSION`); set global=true to fall back to " +
			"the session-option defaults (`-g`). scope=window reports per-window " +
			"options for SESSION:WINDOW (`tmux show-options -w -t SESSION:WINDOW`); " +
			"global=true similarly returns the window-option defaults. The " +
			"response is a flat object mapping option name → value, parsed " +
			"line-by-line from tmux's stdout.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"scope":   map[string]any{"type": "string", "enum": []string{"server", "session", "window"}},
				"session": map[string]any{"type": "string"},
				"window":  map[string]any{"type": "string"},
				"global":  map[string]any{"type": "boolean", "default": false},
			},
			"required": []string{"scope"},
		},
	},
}

func init() {
	// Register show_options onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from a single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, optionsToolDefs...)
}

// showOptions drives [tmuxctl.Controller.ShowOptions] and serialises
// the result to the standard `{"content":[{"type":"text","text":"<json>"}]}`
// envelope MCP expects for tools/call. The output shape is intentionally
// flat — `{ "options": { "key": "value", ... } }` — so future additions
// (e.g. surfacing the resolved scope back to the caller) do not break
// callers that read the fields they care about.
//
// The handler validates required fields per scope before reaching
// tmux: scope is always required; session is required for
// scope=session and scope=window; window is required for scope=window.
// Bad inputs surface as -32602 invalidParams, matching the dispatch
// contract every other boundary tool upholds.
//
// Unknown session/window references surface via the wrapped
// errs.ErrSessionNotFound sentinel, which the JSON-RPC layer maps to
// CodeSessionNotFound (-32000).
func (t *Tools) showOptions(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Scope   string `json:"scope"`
		Session string `json:"session"`
		Window  string `json:"window"`
		Global  bool   `json:"global"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("show_options: %v", err)
	}
	switch args.Scope {
	case tmuxctl.OptionScopeServer:
		// Server-scope ignores session/window entirely — no extra
		// validation needed.
	case tmuxctl.OptionScopeSession:
		if rerr := validateSessionRef(args.Session); rerr != nil {
			return nil, rerr
		}
	case tmuxctl.OptionScopeWindow:
		if rerr := validateSessionRef(args.Session); rerr != nil {
			return nil, rerr
		}
		if rerr := validateWindowTarget(args.Window); rerr != nil {
			return nil, rerr
		}
	case "":
		return nil, invalidParams("show_options: scope required")
	default:
		return nil, invalidParams("show_options: scope %q must be one of server|session|window", args.Scope)
	}
	// scope=session/window targets a specific session; rewrite through
	// the configured -session-prefix the same way every other
	// session-targeting tool does, so a multi-tenant deployment routes
	// the show-options query at the prefixed tmux session the caller
	// actually owns. scope=server ignores session entirely, and an empty
	// session under scope=session/window has already been rejected by
	// validateSessionRef above, so the resolver is a no-op there.
	resolvedSession := t.resolveSessionRef(args.Session)
	options, err := t.Ctl.ShowOptions(ctx, args.Scope, resolvedSession, args.Window, args.Global)
	if err != nil {
		return nil, internalError(err)
	}
	return jsonBlock(map[string]any{
		"options": options,
	})
}

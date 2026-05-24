package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// maxPaneCommandLen caps the optional `command` argument passed to
// pane_split. tmux happily forwards any string to /bin/sh -c, but a
// realistic command rarely exceeds a few hundred bytes — anything past
// 4 KiB is almost certainly a buggy or hostile caller, and bounding it
// here keeps the JSON-RPC frame size predictable.
const maxPaneCommandLen = 4096

// paneTargetRE accepts the tmux pane-target forms the boundary
// understands: bare "session", "session:window", or
// "session:window.pane". The pieces all use the conservative session-
// name policy (alnum/underscore/dash) and the numeric tail is digits
// only. We deliberately leave the deeper validation (does the pane
// exist?) to tmux — the regex is just a cheap up-front guard against
// stray quoting / shell metachars / very long inputs.
var paneTargetRE = regexp.MustCompile(`^[A-Za-z0-9_-]+(:[0-9]+(\.[0-9]+)?)?$`)

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
			"additionalProperties": false,
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
			"required":             []string{"target"},
			"additionalProperties": false,
		},
	},
	{
		"name": "pane_split",
		"description": "Split a pane in two via `tmux split-window`. `direction` is required and must be " +
			"either \"horizontal\" (-h, side-by-side) or \"vertical\" (-v, stacked). " +
			"`target_pane` accepts any tmux target form (\"session\", \"session:window\", " +
			"\"session:window.pane\"); when omitted, the session's currently active pane is split. " +
			"`command` runs in the new pane; defaults to the user's shell when blank. `detach` " +
			"(default false) keeps focus on the original pane (`-d`) so an agent can keep typing " +
			"into it. Returns the new pane's id (e.g. \"%5\") and 0-based index.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "Existing session id; len 1-64, [A-Za-z0-9_-].",
				},
				"target_pane": map[string]any{
					"type":        "string",
					"description": "Optional pane target (\"session\", \"session:window\", or \"session:window.pane\").",
				},
				"direction": map[string]any{
					"type":        "string",
					"enum":        []string{"horizontal", "vertical"},
					"description": "Split axis: horizontal (-h) is side-by-side, vertical (-v) is stacked.",
				},
				"command": map[string]any{
					"type":        "string",
					"description": "Optional initial command; defaults to the user's shell.",
				},
				"detach": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, focus stays on the original pane (-d).",
				},
			},
			"required":             []string{"session", "direction"},
			"additionalProperties": false,
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

// validatePaneTarget enforces the up-front guard for the optional
// `target_pane` argument. Empty is allowed (the controller falls back
// to splitting the session's active pane); a non-empty value must
// match the conservative paneTargetRE so we never let a stray quote
// or path-injection slip through to tmux.
func validatePaneTarget(target string) *rpcError {
	if target == "" {
		return nil
	}
	if len(target) > maxSessionNameLen*2 {
		return invalidParams("target_pane length %d out of range", len(target))
	}
	if !paneTargetRE.MatchString(target) {
		return invalidParams("target_pane %q must match %s", target, paneTargetRE.String())
	}
	return nil
}

// paneSplit drives tmuxctl.Controller.SplitPane. The handler does the
// usual up-front validation (session regex, direction whitelist,
// command size cap, optional pane-target shape) so a caller passing a
// malformed direction sees CodeInvalidParams (-32602) before any tmux
// command runs. The response is a JSON block carrying the new pane's
// id and 0-based index, ready for follow-up pane_select / send_keys.
func (t *Tools) paneSplit(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session    string `json:"session"`
		TargetPane string `json:"target_pane"`
		Direction  string `json:"direction"`
		Command    string `json:"command"`
		Detach     bool   `json:"detach"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("pane_split: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if rerr := validatePaneTarget(args.TargetPane); rerr != nil {
		return nil, rerr
	}
	switch args.Direction {
	case "horizontal", "vertical":
		// ok
	case "":
		return nil, invalidParams("pane_split: direction required (one of \"horizontal\", \"vertical\")")
	default:
		return nil, invalidParams("pane_split: direction %q must be \"horizontal\" or \"vertical\"", args.Direction)
	}
	if len(args.Command) > maxPaneCommandLen {
		return nil, invalidParams("pane_split: command length %d exceeds %d", len(args.Command), maxPaneCommandLen)
	}
	res, err := t.Ctl.SplitPane(ctx, tmuxctl.SplitOptions{
		Session:    args.Session,
		TargetPane: args.TargetPane,
		Direction:  args.Direction,
		Command:    args.Command,
		Detach:     args.Detach,
	})
	if err != nil {
		return nil, internalError(fmt.Errorf("pane_split: %w", err))
	}
	return jsonBlock(map[string]any{
		"id":    res.ID,
		"index": res.Index,
	})
}

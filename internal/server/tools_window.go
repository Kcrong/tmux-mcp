package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// windowToolDefs holds the JSON Schemas for the window-management
// tools. The block is appended onto the main toolDefs slice from the
// init() in this file so the registration site stays close to the
// handlers — the dispatcher in tools.go only needs the two name →
// handler entries.
var windowToolDefs = []map[string]any{
	{
		"name": "window_create",
		"description": "Create a new window inside an existing tmux session via `tmux new-window`. The optional " +
			"`name` is the human-readable label (`-n`); when omitted, tmux auto-assigns one from the " +
			"command. `command` runs in the new window (defaults to the user's shell). `select` " +
			"(default true) controls whether tmux focuses the new window — set false to create it in " +
			"the background (`-d`). Returns a text block confirming the window name (or numeric index " +
			"if no name was supplied) and the session it was created in.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "Existing session name; len 1-64, [A-Za-z0-9_-].",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Optional window name; len 1-64, [A-Za-z0-9_-].",
				},
				"command": map[string]any{
					"type":        "string",
					"description": "Optional initial command; defaults to the user's shell.",
				},
				"select": map[string]any{
					"type":        "boolean",
					"default":     true,
					"description": "When true (default), tmux focuses the new window. False creates it in the background (-d).",
				},
			},
			"required": []string{"session"},
		},
	},
	{
		"name": "window_kill",
		"description": "Destroy a single window in a session via `tmux kill-window -t <session>:<window>`. " +
			"`window` may be a name (1-64, [A-Za-z0-9_-]) or a numeric index. The call is refused " +
			"with -32602 (invalid params) when the targeted window is the only window left in the " +
			"session — use session_kill instead in that case to avoid blurring the boundary " +
			"between window_kill and session_kill.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "Existing session name; len 1-64, [A-Za-z0-9_-].",
				},
				"window": map[string]any{
					"type":        "string",
					"description": "Window name (len 1-64, [A-Za-z0-9_-]) or numeric index (\\d+).",
				},
			},
			"required": []string{"session", "window"},
		},
	},
	{
		"name": "list_windows",
		"description": "Enumerate windows visible to this server. Pass `session` to scope the listing to a " +
			"single tmux session; omit it to list every window on the server (-a). Each entry " +
			"includes the window index, name, active flag, and pane count, so callers can build " +
			"a `session:index` target for follow-up window_kill / send_keys / capture calls.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"maxLength":   maxSessionNameLen,
					"description": "Optional session name; len 1-64, [A-Za-z0-9_-]. Omit to list every window.",
				},
			},
			// list_windows takes only the optional `session` arg today.
			// Locking additionalProperties keeps the schema strict so an
			// agent that misnames a field gets a fast schema-shaped
			// rejection rather than a silent no-op.
			"additionalProperties": false,
		},
	},
}

// windowNameRE mirrors sessionNameRE so window names share the same
// conservative alnum/underscore/dash policy. Re-stating the regex here
// (instead of reusing sessionNameRE directly) keeps the public surface
// of validate.go untouched while documenting that the rule is the same.
var windowNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// windowTargetRE accepts either a window name (matching windowNameRE)
// or a pure numeric tmux window index. tmux resolves both forms
// uniformly via `kill-window -t <session>:<target>`.
var windowTargetRE = regexp.MustCompile(`^([A-Za-z0-9_-]+|[0-9]+)$`)

// maxWindowNameLen pins the upper length bound on window-related
// strings. tmux happily accepts longer names but they make CLI output
// hard to read and rarely reflect a deliberate choice — bound to the
// same value as the session name policy for consistency.
const maxWindowNameLen = 64

// validateWindowName enforces the conservative window-name policy used
// for window_create's optional `name` argument. Empty is allowed at
// the boundary (the handler skips -n when nothing was supplied); the
// regex/length rules only fire when a value is present.
func validateWindowName(name string) *rpcError {
	if name == "" {
		return nil
	}
	if len(name) > maxWindowNameLen {
		return invalidParams("window name length %d out of range [1..%d]", len(name), maxWindowNameLen)
	}
	if !windowNameRE.MatchString(name) {
		return invalidParams("window name %q must match %s", name, windowNameRE.String())
	}
	return nil
}

// validateWindowTarget enforces the policy on window_kill's `window`
// argument. Unlike validateWindowName, an empty value is rejected up
// front because the schema marks it required.
func validateWindowTarget(target string) *rpcError {
	if target == "" {
		return invalidParams("window required")
	}
	if len(target) > maxWindowNameLen {
		return invalidParams("window length %d out of range [1..%d]", len(target), maxWindowNameLen)
	}
	if !windowTargetRE.MatchString(target) {
		return invalidParams("window %q must match %s", target, windowTargetRE.String())
	}
	return nil
}

func init() {
	// Register the window tools onto the main toolDefs slice from this
	// file's init() so the registration site stays close to the
	// handlers and the shared toolDefs literal in tools.go stays small.
	toolDefs = append(toolDefs, windowToolDefs...)
}

// windowCreate drives tmuxctl.Controller.CreateWindow. Validates the
// session reference, the optional window name, and the boolean default
// for `select` before any tmux command runs. Returns a human-readable
// text block summarising what was created.
func (t *Tools) windowCreate(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
		Name    string `json:"name"`
		Command string `json:"command"`
		// *bool so we can distinguish "select absent (default true)" from
		// "select=false (explicit -d)". The schema's default of true is
		// applied when the field was missing or null.
		Select *bool `json:"select"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("window_create: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if rerr := validateWindowName(args.Name); rerr != nil {
		return nil, rerr
	}
	sel := true
	if args.Select != nil {
		sel = *args.Select
	}
	res, err := t.Ctl.CreateWindow(ctx, tmuxctl.WindowSpec{
		Session: args.Session,
		Name:    args.Name,
		Command: args.Command,
		Select:  sel,
	})
	if err != nil {
		return nil, internalError(err)
	}
	// Prefer the human-readable name when one is set; fall back to the
	// numeric index for windows tmux auto-named (no -n was passed) so
	// the response always carries something the caller can target with
	// a follow-up window_kill.
	label := res.Name
	if label == "" {
		label = res.Index
	}
	return textBlock(fmt.Sprintf("window %q created in %q", label, res.Session)), nil
}

// windowKill drives tmuxctl.Controller.KillWindow. Up-front it
// validates the session and target, then refuses with CodeInvalidParams
// when the targeted window would be the last one in its session — that
// case is reserved for session_kill so the two tools' semantics stay
// distinct.
func (t *Tools) windowKill(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
		Window  string `json:"window"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("window_kill: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if rerr := validateWindowTarget(args.Window); rerr != nil {
		return nil, rerr
	}
	// Pre-flight: refuse to kill the only window of a session. tmux
	// would otherwise tear down the session itself, which agents would
	// find surprising — and which session_kill is the explicit way to
	// request anyway.
	count, err := t.Ctl.CountWindows(ctx, args.Session)
	if err != nil {
		return nil, internalError(err)
	}
	if count <= 1 {
		return nil, invalidParams(
			"cannot kill the only remaining window; use session_kill instead",
		)
	}
	if err := t.Ctl.KillWindow(ctx, args.Session, args.Window); err != nil {
		return nil, internalError(err)
	}
	return textBlock(fmt.Sprintf("window %q killed", args.Session+":"+args.Window)), nil
}

// listWindows drives tmuxctl.Controller.ListWindows and serialises the
// result to the standard `{"content":[{"type":"text","text":"<json>"}]}`
// envelope MCP expects from a tools/call. The response shape is a flat
// object keyed by "windows" so a future filter (e.g. "active_only" or
// a "scope" knob) can be added without breaking callers that iterate
// the list.
//
// `session` is optional: when present it must satisfy the same regex /
// length policy as every other session reference; when absent the
// listing covers every window on the server (the -a branch). Unknown
// session names surface via the wrapped errs.ErrSessionNotFound which
// the JSON-RPC layer maps to CodeSessionNotFound.
func (t *Tools) listWindows(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}` here, and the zero value of args.Session means
	// "list every window on the server".
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("list_windows: %v", err)
		}
	}
	if args.Session != "" {
		if rerr := validateSessionRef(args.Session); rerr != nil {
			return nil, rerr
		}
	}
	wins, err := t.Ctl.ListWindows(ctx, args.Session)
	if err != nil {
		return nil, internalError(err)
	}
	out := make([]map[string]any, 0, len(wins))
	for _, w := range wins {
		out = append(out, map[string]any{
			"index":  w.Index,
			"name":   w.Name,
			"active": w.Active,
			"panes":  w.Panes,
		})
	}
	return jsonBlock(map[string]any{"windows": out})
}

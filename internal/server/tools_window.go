package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

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
	{
		"name": "window_select",
		"description": "Make `target` the active window of `session` via `tmux select-window`. `target` " +
			"may be a window name (1-64, [A-Za-z0-9_-]) or numeric index. Subsequent send_keys / " +
			"capture calls that name the session will then act on the newly focused window. Pair " +
			"with list_windows to discover the available targets.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "Existing session name; len 1-64, [A-Za-z0-9_-].",
				},
				"target": map[string]any{
					"type":        "string",
					"description": "Window name (len 1-64, [A-Za-z0-9_-]) or numeric index (\\d+).",
				},
			},
			"required":             []string{"session", "target"},
			"additionalProperties": false,
		},
	},
	{
		"name": "window_rename",
		"description": "Rename a window via `tmux rename-window -t <session>:<target> <name>`. `target` " +
			"may be a window name (1-64, [A-Za-z0-9_-]) or numeric index; `name` is the new " +
			"label and must satisfy the same conservative regex/length policy as window_create.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "Existing session name; len 1-64, [A-Za-z0-9_-].",
				},
				"target": map[string]any{
					"type":        "string",
					"description": "Window name (len 1-64, [A-Za-z0-9_-]) or numeric index (\\d+).",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "New window name; len 1-64, [A-Za-z0-9_-].",
				},
			},
			"required":             []string{"session", "target", "name"},
			"additionalProperties": false,
		},
	},
	{
		"name": "window_move",
		"description": "Move a window via `tmux move-window -s <src> -t <dst>`. `src` is the source " +
			"target in tmux `<session>:<window>` form (e.g. `demo:0`); `dst` is the destination " +
			"target in the same form (e.g. `demo:5`) and may carry an empty window part " +
			"(e.g. `archive:`) to let tmux pick the next available index in the destination " +
			"session. Useful for renumbering a window inside a session or relocating it onto " +
			"another session this server already manages.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"src": map[string]any{
					"type":        "string",
					"description": "Source target like `mysession:0`; session 1-64, window name (1-64, [A-Za-z0-9_-]) or numeric index.",
				},
				"dst": map[string]any{
					"type":        "string",
					"description": "Destination target like `mysession:5` or `othersession:` (empty window part lets tmux pick).",
				},
			},
			"required":             []string{"src", "dst"},
			"additionalProperties": false,
		},
	},
	{
		"name": "swap_window",
		"description": "Exchange two windows of the same session in place via " +
			"`tmux swap-window -s <session>:<src> -t <session>:<dst>`. tmux trades the layout " +
			"slots: each window keeps its `#{window_id}`, contents, panes, and running " +
			"processes while the position indices/names trade. `src` and `dst` may be window " +
			"names (1-64, `^[A-Za-z0-9_-]+$`) or numeric indices (`\\d+`); they must differ. " +
			"`no_select` (default false) maps to tmux's `-d` flag — when true, the session's " +
			"active window pointer is left alone after the swap, so a chained send_keys / " +
			"capture stays deterministic. Pairs with pane_swap (panes inside a window) and " +
			"window_move (cross-session relocation).",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "Existing session name; len 1-64, [A-Za-z0-9_-].",
				},
				"src": map[string]any{
					"type":        "string",
					"description": "Source window name (len 1-64, [A-Za-z0-9_-]) or numeric index (\\d+).",
				},
				"dst": map[string]any{
					"type":        "string",
					"description": "Destination window name (len 1-64, [A-Za-z0-9_-]) or numeric index (\\d+).",
				},
				"no_select": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, do not change the active window after the swap (`-d`).",
				},
			},
			"required":             []string{"session", "src", "dst"},
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

// validateRequiredWindowName mirrors validateWindowName but rejects the
// empty value up front. window_rename's `name` argument is required (a
// rename to "" is meaningless), so the handler reuses this stricter
// variant rather than open-coding the empty check on top of the
// optional one.
func validateRequiredWindowName(name string) *rpcError {
	if name == "" {
		return invalidParams("window name required")
	}
	return validateWindowName(name)
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

// validateWindowMoveSrc enforces the policy on window_move's `src`
// argument. The src must be a complete `<session>:<window>` reference
// because tmux's `move-window -s` needs an unambiguous source — leaving
// the window part empty would let tmux pick the "current" window of the
// session, which is rarely what an agent meant.
func validateWindowMoveSrc(src string) *rpcError {
	if src == "" {
		return invalidParams("src required")
	}
	idx := strings.Index(src, ":")
	if idx < 0 {
		return invalidParams("src %q must be in `<session>:<window>` form", src)
	}
	session := src[:idx]
	window := src[idx+1:]
	if rerr := validateSessionRef(session); rerr != nil {
		return invalidParams("src session: %s", rerr.Message)
	}
	if rerr := validateWindowTarget(window); rerr != nil {
		return invalidParams("src window: %s", rerr.Message)
	}
	return nil
}

// validateWindowMoveDst enforces the policy on window_move's `dst`
// argument. dst must include the `:` separator so a typo like
// "othersession" cannot accidentally be parsed as a session-only
// target; the window part *is* allowed to be empty (e.g.
// "othersession:") to let tmux pick the next available index in the
// destination session — that is one of move-window's documented modes.
func validateWindowMoveDst(dst string) *rpcError {
	if dst == "" {
		return invalidParams("dst required")
	}
	idx := strings.Index(dst, ":")
	if idx < 0 {
		return invalidParams("dst %q must be in `<session>:<window>` form", dst)
	}
	session := dst[:idx]
	window := dst[idx+1:]
	if rerr := validateSessionRef(session); rerr != nil {
		return invalidParams("dst session: %s", rerr.Message)
	}
	// Empty window part is intentionally allowed — tmux interprets
	// `<session>:` as "pick the next free index in <session>". When
	// supplied, the value must satisfy the same regex/length policy as
	// every other window target.
	if window != "" {
		if rerr := validateWindowTarget(window); rerr != nil {
			return invalidParams("dst window: %s", rerr.Message)
		}
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
		Session: t.resolveSessionRef(args.Session),
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
	// a follow-up window_kill. Echo the logical session name (what the
	// client passed) instead of res.Session, so a -session-prefix
	// deployment doesn't leak the prefixed identity back to the caller.
	label := res.Name
	if label == "" {
		label = res.Index
	}
	return textBlock(fmt.Sprintf("window %q created in %q", label, args.Session)), nil
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
	resolved := t.resolveSessionRef(args.Session)
	count, err := t.Ctl.CountWindows(ctx, resolved)
	if err != nil {
		return nil, internalError(err)
	}
	if count <= 1 {
		return nil, invalidParams(
			"cannot kill the only remaining window; use session_kill instead",
		)
	}
	if err := t.Ctl.KillWindow(ctx, resolved, args.Window); err != nil {
		return nil, internalError(err)
	}
	// Echo the logical session name in the response so a -session-prefix
	// deployment never leaks the prefixed identity to the caller.
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
	// Apply -session-prefix when scoping to a single session so we hit
	// the actual tmux session the rest of the surface addresses. Empty
	// session preserves the unscoped (-a) listing path.
	wins, err := t.Ctl.ListWindows(ctx, t.resolveSessionRef(args.Session))
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

// windowSelect drives tmuxctl.Controller.SelectWindow. The handler
// validates session and target up front so a malformed reference fails
// fast with -32602 before any tmux command runs. On success the
// response is the same trivial "ok" status text block pane_select uses
// — callers chain into list_windows / capture if they want to confirm
// the active flag actually moved.
func (t *Tools) windowSelect(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
		Target  string `json:"target"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("window_select: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if rerr := validateWindowTarget(args.Target); rerr != nil {
		return nil, rerr
	}
	if err := t.Ctl.SelectWindow(ctx, t.resolveSessionRef(args.Session), args.Target); err != nil {
		return nil, internalError(err)
	}
	return textBlock("ok"), nil
}

// windowRename drives tmuxctl.Controller.RenameWindow. Validates the
// session reference, the existing window target, and the *new* window
// name before any tmux command runs. The new name shares the same
// conservative regex/length policy window_create uses for its optional
// `name`, so the rename surface stays consistent with creation.
func (t *Tools) windowRename(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
		Target  string `json:"target"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("window_rename: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if rerr := validateWindowTarget(args.Target); rerr != nil {
		return nil, rerr
	}
	if rerr := validateRequiredWindowName(args.Name); rerr != nil {
		return nil, rerr
	}
	if err := t.Ctl.RenameWindow(ctx, t.resolveSessionRef(args.Session), args.Target, args.Name); err != nil {
		return nil, internalError(err)
	}
	// Echo the logical session name so a -session-prefix deployment
	// never leaks the prefixed identity to the caller.
	return textBlock(fmt.Sprintf("window %q renamed to %q", args.Session+":"+args.Target, args.Name)), nil
}

// windowMove drives tmuxctl.Controller.MoveWindow. Both `src` and `dst`
// arrive as full tmux target strings (`<session>:<window>`); the
// boundary parses each on the `:` separator, applies the standard
// session and window-target regex/length policy to each half, and only
// then asks tmux to perform the move. Empty window parts are tolerated
// in `dst` (lets tmux pick the next free index in the destination
// session) but not in `src`, which would otherwise resolve to whatever
// tmux considers the session's current window.
func (t *Tools) windowMove(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Src string `json:"src"`
		Dst string `json:"dst"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("window_move: %v", err)
	}
	if rerr := validateWindowMoveSrc(args.Src); rerr != nil {
		return nil, rerr
	}
	if rerr := validateWindowMoveDst(args.Dst); rerr != nil {
		return nil, rerr
	}
	if err := t.Ctl.MoveWindow(ctx,
		t.resolveWindowMoveTarget(args.Src),
		t.resolveWindowMoveTarget(args.Dst),
	); err != nil {
		return nil, internalError(err)
	}
	// Echo the logical (un-prefixed) src/dst the caller passed so a
	// -session-prefix deployment never leaks the prefixed identity.
	return textBlock(fmt.Sprintf("window %q moved to %q", args.Src, args.Dst)), nil
}

// swapWindow drives tmuxctl.Controller.SwapWindow. Validates the
// session reference and the two window targets up front so a malformed
// call sees CodeInvalidParams (-32602) before any tmux command runs;
// src and dst must also differ — letting tmux be the one to refuse a
// no-op swap would emit a less informative error than the boundary's
// own "src and dst must differ" message.
//
// no_select is parsed as a *bool so the schema's documented default of
// false is applied identically whether the field was missing, null, or
// explicitly false. The handler returns a small JSON ack
// `{"swapped": true}` on success — tmux's swap-window itself produces
// no useful stdout, and chained list_windows is one call away if the
// caller wants to confirm the layout.
//
// A missing session/window surfaces as CodeSessionNotFound (-32000)
// via internalError → errs.CodeOf, mirroring window_select /
// window_move / window_rename.
func (t *Tools) swapWindow(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session  string `json:"session"`
		Src      string `json:"src"`
		Dst      string `json:"dst"`
		NoSelect *bool  `json:"no_select"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("swap_window: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if rerr := validateWindowTarget(args.Src); rerr != nil {
		return nil, invalidParams("src: %s", rerr.Message)
	}
	if rerr := validateWindowTarget(args.Dst); rerr != nil {
		return nil, invalidParams("dst: %s", rerr.Message)
	}
	if args.Src == args.Dst {
		return nil, invalidParams("src and dst must differ")
	}
	noSelect := false
	if args.NoSelect != nil {
		noSelect = *args.NoSelect
	}
	if err := t.Ctl.SwapWindow(ctx,
		t.resolveSessionRef(args.Session),
		args.Src, args.Dst, noSelect,
	); err != nil {
		return nil, internalError(err)
	}
	return jsonBlock(map[string]any{"swapped": true})
}

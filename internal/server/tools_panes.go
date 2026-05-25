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

// maxPaneResizeAmount is the upper bound (cells) the pane_resize tool
// accepts in a single call. tmux happily clamps oversized requests
// against the surrounding window, but a realistic resize rarely moves
// the boundary by more than a handful of cells — anything past 200 is
// almost certainly a typo (e.g. pixels mistaken for cells) and capping
// here keeps the boundary predictable for callers that script chains
// of resizes.
const maxPaneResizeAmount = 200

// paneTargetRE accepts the tmux pane-target forms the boundary
// understands: bare "session", "session:window",
// "session:window.pane", or the tmux internal pane id "%N" (e.g.
// "%5") that pane_split returns and the agent can hand straight back
// to pane_kill / pane_select. The pieces all use the conservative
// session-name policy (alnum/underscore/dash) and the numeric tail is
// digits only. We deliberately leave the deeper validation (does the
// pane exist?) to tmux — the regex is just a cheap up-front guard
// against stray quoting / shell metachars / very long inputs.
var paneTargetRE = regexp.MustCompile(`^([A-Za-z0-9_-]+(:[0-9]+(\.[0-9]+)?)?|%[0-9]+)$`)

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
			"will then act on the newly selected pane. For mark/unmark, last-active jumps, " +
			"directional walks, input toggling, or zoom, use `select_pane` — it accepts the same " +
			"target plus the full optional flag set tmux's `select-pane` understands.",
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
	{
		"name": "pane_kill",
		"description": "Destroy a pane via `tmux kill-pane -t <target_pane>`. `target_pane` accepts any tmux " +
			"pane-target form (\"session\", \"session:window\", or \"session:window.pane\"). Mirrors " +
			"the natural tmux semantics: killing the only remaining pane of a window also tears down " +
			"that window, and if it was the only remaining window of a session the session itself is " +
			"reaped — pre-check with list_panes / list_windows when the caller needs to guard against " +
			"that. Returns a small JSON ack `{\"killed\": true}` on success.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "Optional session id (informational only; len 1-64, [A-Za-z0-9_-]).",
				},
				"target_pane": map[string]any{
					"type":        "string",
					"description": "Pane target (\"session\", \"session:window\", or \"session:window.pane\").",
				},
			},
			"required":             []string{"target_pane"},
			"additionalProperties": false,
		},
	},
	{
		"name": "select_pane",
		"description": "Wrap `tmux select-pane -t TARGET` with the full optional flag set. " +
			"Use this in place of `pane_select` when the caller needs to mark / unmark the " +
			"pane (`-m` / `-M`), jump to the last-active pane (`-l`), walk one step toward a " +
			"neighbour (`-U`/`-D`/`-L`/`-R`, picked via `direction`), toggle pane input " +
			"(`-e`/`-d`), or zoom the window on the target (`-Z`) — all atomic on tmux's side. " +
			"`target` is the same tmux pane target form `pane_select` accepts " +
			"(`session`, `session:window`, `session:window.pane`, or the `%N` pane id). " +
			"`mark`/`unmark` and `enable_input`/`disable_input` are mutually exclusive; " +
			"requesting both pairs trips CodeInvalidParams before any tmux command runs. " +
			"Returns `ok` on success.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Pane target (\"session\", \"session:window\", \"session:window.pane\", or `%N`).",
				},
				"mark": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, mark this pane (-m) so swap-pane / join-pane can pick it up implicitly.",
				},
				"unmark": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, clear the marked-pane state (-M).",
				},
				"last": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, jump to the last-active pane (-l) of the target's window.",
				},
				"direction": map[string]any{
					"type":        "string",
					"enum":        []string{"up", "down", "left", "right"},
					"description": "Walk one step toward the named neighbour: up (-U), down (-D), left (-L), right (-R).",
				},
				"enable_input": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, enable input on the pane (-e).",
				},
				"disable_input": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, disable input on the pane (-d).",
				},
				"zoom": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, also zoom the window on the target pane (-Z).",
				},
			},
			"required":             []string{"target"},
			"additionalProperties": false,
		},
	},
	{
		"name": "pane_swap",
		"description": "Exchange two panes in place via `tmux swap-pane -s <src> -t <dst>`. tmux " +
			"swaps the layout slots: each pane keeps its `#{pane_id}`, contents, and " +
			"running process while the positions trade. Both arguments are tmux pane " +
			"targets (e.g. \"demo:0.0\", \"demo:0.1\"). Useful for rearranging a multi-" +
			"pane TUI layout without recreating panes.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"src": map[string]any{
					"type":        "string",
					"description": "Source pane target (e.g. \"session:window.pane\").",
				},
				"dst": map[string]any{
					"type":        "string",
					"description": "Destination pane target (e.g. \"session:window.pane\").",
				},
			},
			"required":             []string{"src", "dst"},
			"additionalProperties": false,
		},
	},
	{
		"name": "pane_join",
		"description": "Move a pane out of its current window and re-attach it to another window via " +
			"`tmux join-pane -s <src> -t <dst>` (with `-h` when horizontal=true). The source " +
			"pane keeps its `#{pane_id}`, contents, and running process — only the layout slot " +
			"changes. `src` is the pane to move (e.g. \"mysession:1.0\"); `dst` is the " +
			"destination window (e.g. \"mysession:0\"). horizontal=true splits the destination " +
			"left/right; the default (false) splits top/bottom, matching tmux's interactive " +
			"default. Useful for consolidating panes from multiple windows back into one.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"src": map[string]any{
					"type":        "string",
					"description": "Source pane target (e.g. \"session:window.pane\").",
				},
				"dst": map[string]any{
					"type":        "string",
					"description": "Destination window target (e.g. \"session:window\").",
				},
				"horizontal": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, split the destination left/right (-h); default is top/bottom.",
				},
			},
			"required":             []string{"src", "dst"},
			"additionalProperties": false,
		},
	},
	{
		"name": "pane_resize",
		"description": "Resize a pane via `tmux resize-pane -t <target> -{U|D|L|R} <amount>`. " +
			"`direction` selects the side the boundary moves toward — \"up\"/\"down\" shift " +
			"the horizontal divider (taller/shorter), \"left\"/\"right\" shift the vertical " +
			"divider (wider/narrower). `amount` is the number of cells to move and must be " +
			"between 1 and 200; tmux clamps requests that would shrink a pane below its " +
			"minimum. Useful for tweaking a multi-pane TUI layout without recreating panes.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Pane target (\"session\", \"session:window\", or \"session:window.pane\").",
				},
				"direction": map[string]any{
					"type":        "string",
					"enum":        []string{"up", "down", "left", "right"},
					"description": "Side the boundary moves toward: up (-U), down (-D), left (-L), right (-R).",
				},
				"amount": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     maxPaneResizeAmount,
					"description": "Number of cells to resize (1-200).",
				},
			},
			"required":             []string{"target", "direction", "amount"},
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
	// Apply -session-prefix when scoping to a single session so the
	// listing lands on the prefixed tmux session the rest of the surface
	// addresses. Empty session keeps the unscoped (-a) listing path
	// intact; cross-prefix entries are not filtered here because the
	// pane list itself carries session:window pairs the caller may need
	// to see verbatim for diagnostics.
	panes, err := t.Ctl.ListPanes(ctx, t.resolveSessionRef(args.Session))
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
	if rerr := validatePaneTarget(args.Target); rerr != nil {
		return nil, invalidParams("pane_select: %s", rerr.Message)
	}
	if err := t.Ctl.SelectPane(ctx, t.resolvePaneTarget(args.Target)); err != nil {
		return nil, internalError(err)
	}
	return textBlock("ok"), nil
}

// selectPane drives tmuxctl.Controller.SelectPaneAdvanced. The handler
// is the more capable sibling of pane_select: it accepts the same target
// form but also forwards the optional flag set tmux's `select-pane`
// understands (mark/unmark, last, directional walk, enable/disable
// input, zoom). The boundary keeps the pane_select handler in place for
// callers that only need the bare "make this pane active" semantics —
// new code that needs any of the flags should call select_pane instead.
//
// Up-front validation mirrors the shape of pane_select / pane_resize:
// non-empty target, conservative regex check, whitelisted direction
// when supplied. The mutual-exclusion guard for mark/unmark and
// enable_input/disable_input lives in the controller so a programmatic
// caller (future ad-hoc internal user) cannot bypass it.
func (t *Tools) selectPane(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target       string `json:"target"`
		Mark         bool   `json:"mark"`
		Unmark       bool   `json:"unmark"`
		Last         bool   `json:"last"`
		Direction    string `json:"direction"`
		EnableInput  bool   `json:"enable_input"`
		DisableInput bool   `json:"disable_input"`
		Zoom         bool   `json:"zoom"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("select_pane: %v", err)
	}
	if args.Target == "" {
		return nil, invalidParams("select_pane: target required")
	}
	if rerr := validatePaneTarget(args.Target); rerr != nil {
		return nil, invalidParams("select_pane: %s", rerr.Message)
	}
	switch args.Direction {
	case "", "up", "down", "left", "right":
		// ok
	default:
		return nil, invalidParams("select_pane: direction %q must be one of \"up\", \"down\", \"left\", \"right\"", args.Direction)
	}
	if args.Mark && args.Unmark {
		return nil, invalidParams("select_pane: mark and unmark are mutually exclusive")
	}
	if args.EnableInput && args.DisableInput {
		return nil, invalidParams("select_pane: enable_input and disable_input are mutually exclusive")
	}
	err := t.Ctl.SelectPaneAdvanced(ctx, tmuxctl.SelectPaneOptions{
		Target:       t.resolvePaneTarget(args.Target),
		Mark:         args.Mark,
		Unmark:       args.Unmark,
		Last:         args.Last,
		Direction:    args.Direction,
		EnableInput:  args.EnableInput,
		DisableInput: args.DisableInput,
		Zoom:         args.Zoom,
	})
	if err != nil {
		return nil, internalError(fmt.Errorf("select_pane: %w", err))
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
		Session:    t.resolveSessionRef(args.Session),
		TargetPane: t.resolvePaneTarget(args.TargetPane),
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

// paneKill drives tmuxctl.Controller.KillPane. The handler validates
// the optional `session` reference (when supplied) and the required
// `target_pane` shape up front so a caller passing a malformed value
// sees CodeInvalidParams (-32602) before any tmux command runs. The
// response is a small JSON ack `{"killed": true}`; the boundary
// deliberately does not expose whether the kill collapsed the window
// or session — that information is one list_panes / list_windows call
// away if the caller actually needs it.
func (t *Tools) paneKill(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session    string `json:"session"`
		TargetPane string `json:"target_pane"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("pane_kill: %v", err)
	}
	// session is informational here (the target string already pins the
	// pane) — only validate when the caller bothered to supply it, so an
	// agent that already has a fully-qualified target_pane doesn't have
	// to redundantly repeat the session name.
	if args.Session != "" {
		if rerr := validateSessionRef(args.Session); rerr != nil {
			return nil, rerr
		}
	}
	if args.TargetPane == "" {
		return nil, invalidParams("pane_kill: target_pane required")
	}
	if rerr := validatePaneTarget(args.TargetPane); rerr != nil {
		return nil, rerr
	}
	if err := t.Ctl.KillPane(ctx, t.resolvePaneTarget(args.TargetPane)); err != nil {
		return nil, internalError(fmt.Errorf("pane_kill: %w", err))
	}
	return jsonBlock(map[string]any{"killed": true})
}

// paneSwap drives tmuxctl.Controller.SwapPane. Both `src` and `dst`
// must be non-empty pane targets that pass the same conservative regex
// applied everywhere else on the boundary — the controller refuses
// stray quoting / shell metachars before any tmux command runs. A
// missing session surfaces as CodeSessionNotFound (-32000) via
// internalError → errs.CodeOf, mirroring pane_select / pane_split.
func (t *Tools) paneSwap(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Src string `json:"src"`
		Dst string `json:"dst"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("pane_swap: %v", err)
	}
	if args.Src == "" {
		return nil, invalidParams("pane_swap: src required")
	}
	if args.Dst == "" {
		return nil, invalidParams("pane_swap: dst required")
	}
	if rerr := validatePaneTarget(args.Src); rerr != nil {
		return nil, invalidParams("pane_swap: src: %s", rerr.Message)
	}
	if rerr := validatePaneTarget(args.Dst); rerr != nil {
		return nil, invalidParams("pane_swap: dst: %s", rerr.Message)
	}
	if err := t.Ctl.SwapPane(ctx,
		t.resolvePaneTarget(args.Src),
		t.resolvePaneTarget(args.Dst),
	); err != nil {
		return nil, internalError(fmt.Errorf("pane_swap: %w", err))
	}
	return textBlock("ok"), nil
}

// paneJoin drives tmuxctl.Controller.JoinPane. Both `src` and `dst`
// must be non-empty pane targets that pass the same conservative regex
// applied everywhere else on the boundary — the controller refuses
// stray quoting / shell metachars before any tmux command runs.
// `horizontal` is optional and defaults to false; true reaches tmux as
// `-h` (left/right split). A missing session/window/pane surfaces as
// CodeSessionNotFound (-32000) via internalError → errs.CodeOf,
// mirroring pane_swap / pane_split.
func (t *Tools) paneJoin(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Src        string `json:"src"`
		Dst        string `json:"dst"`
		Horizontal bool   `json:"horizontal"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("pane_join: %v", err)
	}
	if args.Src == "" {
		return nil, invalidParams("pane_join: src required")
	}
	if args.Dst == "" {
		return nil, invalidParams("pane_join: dst required")
	}
	if rerr := validatePaneTarget(args.Src); rerr != nil {
		return nil, invalidParams("pane_join: src: %s", rerr.Message)
	}
	if rerr := validatePaneTarget(args.Dst); rerr != nil {
		return nil, invalidParams("pane_join: dst: %s", rerr.Message)
	}
	if err := t.Ctl.JoinPane(ctx, args.Src, args.Dst, args.Horizontal); err != nil {
		return nil, internalError(fmt.Errorf("pane_join: %w", err))
	}
	return textBlock("ok"), nil
}

// paneResize drives tmuxctl.Controller.ResizePane. The handler does the
// usual up-front validation: target must be non-empty and pass the
// pane-target regex, direction must be one of the four whitelisted
// values, and amount must lie within [1..maxPaneResizeAmount] so a
// caller that mistook pixels for cells (or smuggled a negative through
// JSON) trips CodeInvalidParams before any tmux command runs. A
// missing pane surfaces as CodeSessionNotFound (-32000) via
// internalError → errs.CodeOf, mirroring pane_swap / pane_kill.
func (t *Tools) paneResize(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target    string `json:"target"`
		Direction string `json:"direction"`
		Amount    int    `json:"amount"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("pane_resize: %v", err)
	}
	if args.Target == "" {
		return nil, invalidParams("pane_resize: target required")
	}
	if rerr := validatePaneTarget(args.Target); rerr != nil {
		return nil, invalidParams("pane_resize: target: %s", rerr.Message)
	}
	switch args.Direction {
	case "up", "down", "left", "right":
		// ok
	case "":
		return nil, invalidParams("pane_resize: direction required (one of \"up\", \"down\", \"left\", \"right\")")
	default:
		return nil, invalidParams("pane_resize: direction %q must be one of \"up\", \"down\", \"left\", \"right\"", args.Direction)
	}
	if args.Amount < 1 || args.Amount > maxPaneResizeAmount {
		return nil, invalidParams("pane_resize: amount %d out of range [1..%d]", args.Amount, maxPaneResizeAmount)
	}
	if err := t.Ctl.ResizePane(ctx, t.resolvePaneTarget(args.Target), args.Direction, args.Amount); err != nil {
		return nil, internalError(fmt.Errorf("pane_resize: %w", err))
	}
	return textBlock("ok"), nil
}

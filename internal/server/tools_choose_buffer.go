package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// maxChooseBufferFormatLen caps the up-front length check applied to
// `format`, `filter`, `key_format`, and `template` arguments. tmux
// happily accepts very long format strings (the DSL is recursive) but
// a realistic agent rarely needs more than a few hundred bytes —
// anything past 4 KiB is almost certainly a typo or hostile caller,
// and bounding here keeps the JSON-RPC frame size predictable.
const maxChooseBufferFormatLen = 4096

// chooseBufferSortOrders pins the sort-order whitelist tmux's
// choose-buffer accepts via `-O`. tmux 3.4 honours these four values
// (see the tmux(1) man page); we surface them verbatim through the
// schema's `enum` so a typo gets a fast -32602 rejection rather than
// a confusing tmux stderr per call.
//
// A future tmux version that adds new sort modes only needs the table
// and the schema enum updated in lock-step — the dispatcher's switch
// is built off the same slice, so a regression where the validator
// drifts from the schema is impossible.
var chooseBufferSortOrders = []string{"time", "name", "size"}

// chooseBufferKeyFormatRE accepts the conservative shape we permit on
// the optional `key_format` argument: alnum, plus the punctuation
// tmux's key-name DSL actually uses ("Q", "C-c", "M-1", "Down",
// "F2", ...). We deliberately reject whitespace, quotes, and shell
// metachars — none of those are valid inside a tmux key descriptor,
// and letting them through would invite argv-injection if a future
// tmux version starts treating them as a separator the way it
// already does for sessions/windows.
var chooseBufferKeyFormatRE = regexp.MustCompile(`^[A-Za-z0-9._\-]+$`)

// chooseBufferToolDefs holds the JSON Schema for the choose_buffer
// tool. It is appended onto the main toolDefs slice from this file's
// init() so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
var chooseBufferToolDefs = []map[string]any{
	{
		"name": "choose_buffer",
		"description": "Open tmux's interactive paste-buffer chooser inside the target pane via " +
			"`tmux choose-buffer`. The pane enters buffer-mode (`#{?pane_in_mode,1,0}` " +
			"flips to 1 and `#{pane_mode}` reads `buffer-mode`) so a follow-up " +
			"`send_keys` (or a real client attached to the server) can step through the " +
			"buffer list. All arguments are optional: omit `target` to let tmux pick the " +
			"current pane; pass `format` / `filter` / `key_format` / `sort_order` to " +
			"shape the chooser's row presentation; pass `template` (positional in the " +
			"underlying CLI) for the command tmux runs against the selected buffer " +
			"(e.g. `paste-buffer -b %%`). Boolean toggles `no_preview`, `zoom`, and " +
			"`reverse` map to tmux's `-N`, `-Z`, `-r` flags respectively.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Optional pane target (`session`, `session:window`, `session:window.pane`, or `%N`). Omitted lets tmux resolve the current pane.",
				},
				"format": map[string]any{
					"type":        "string",
					"description": "Optional `-F FORMAT` template tmux renders for each row in the chooser (e.g. `#{buffer_name}`). Must not contain newlines.",
				},
				"filter": map[string]any{
					"type":        "string",
					"description": "Optional `-f FILTER` Boolean format that prunes the row set (e.g. `#{>=:#{buffer_size},10}`). Must not contain newlines.",
				},
				"key_format": map[string]any{
					"type":        "string",
					"description": "Optional `-K KEY-FORMAT` per-row key shortcut (alnum / dot / dash / underscore).",
				},
				"sort_order": map[string]any{
					"type":        "string",
					"enum":        chooseBufferSortOrders,
					"description": "Optional `-O SORT-ORDER` for the chooser's row order; one of \"time\", \"name\", \"size\".",
				},
				"template": map[string]any{
					"type":        "string",
					"description": "Optional positional TEMPLATE — the tmux command run against the selected buffer (e.g. `paste-buffer -b %%`). Must not contain newlines.",
				},
				"no_preview": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, pass `-N` so the chooser does not render a preview of the selected buffer.",
				},
				"zoom": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, pass `-Z` so the chooser pane is zoomed for the duration of the picker.",
				},
				"reverse": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, pass `-r` so the chooser lists rows in reverse sort order.",
				},
			},
			// choose_buffer's surface is locked to the (target, format,
			// filter, key_format, sort_order, template, no_preview, zoom,
			// reverse) tuple today; an unknown field is far more likely
			// a typo than a future capability we forgot to advertise, so
			// reject it up front rather than silently ignore it.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register choose_buffer onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from the single dispatcher case in tools.go)
	// and avoids touching the shared toolDefs literal that other PRs
	// are editing. The read-only allowlist membership lives in
	// readonly.go alongside the other inspection-only tool names.
	toolDefs = append(toolDefs, chooseBufferToolDefs...)
}

// validateChooseBufferFormat enforces the up-front policy on the
// optional format-style strings (`format`, `filter`, `template`).
// tmux silently joins multi-line formats with embedded newlines,
// which would split the JSON-RPC frame budget and produce values the
// schema documents as a single line. Empty input is allowed (the
// caller forwards a possibly-empty CLI flag without an extra branch);
// non-empty values must not contain CR/LF and must fit within the
// shared length cap.
func validateChooseBufferFormat(field, value string) *rpcError {
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "\n\r") {
		return invalidParams("%s: must not contain newlines", field)
	}
	if len(value) > maxChooseBufferFormatLen {
		return invalidParams("%s length %d exceeds %d", field, len(value), maxChooseBufferFormatLen)
	}
	return nil
}

// validateChooseBufferKeyFormat enforces the up-front policy on the
// optional `key_format` argument. tmux's key-name DSL is restrictive
// (alnum, dot, dash, underscore — see the tmux(1) man page on
// "Key bindings"), so we mirror the same character class and reject
// anything that would be invalid as a key descriptor anyway.
func validateChooseBufferKeyFormat(value string) *rpcError {
	if value == "" {
		return nil
	}
	if len(value) > maxChooseBufferFormatLen {
		return invalidParams("key_format length %d exceeds %d", len(value), maxChooseBufferFormatLen)
	}
	if !chooseBufferKeyFormatRE.MatchString(value) {
		return invalidParams("key_format %q must match %s", value, chooseBufferKeyFormatRE.String())
	}
	return nil
}

// validateChooseBufferSortOrder enforces the whitelist on the
// optional `sort_order` argument. The schema's enum already filters
// this surface, but we keep a defensive check so a hand-crafted call
// that bypasses schema validation sees a fast -32602 instead of an
// obscure tmux stderr per call.
func validateChooseBufferSortOrder(value string) *rpcError {
	if value == "" {
		return nil
	}
	for _, allowed := range chooseBufferSortOrders {
		if value == allowed {
			return nil
		}
	}
	return invalidParams("sort_order %q must be one of %s", value, strings.Join(chooseBufferSortOrders, "|"))
}

// chooseBuffer drives tmuxctl.Controller.ChooseBuffer and serialises
// the result to the standard `{"content":[{"type":"text","text":"<json>"}]}`
// envelope MCP expects from a tools/call. The response shape is a
// flat object keyed by `entered` so a future addition (e.g. echoing
// the resolved target back to the caller) does not break callers
// that only branch on the boolean.
//
// Up-front validation:
//   - `target`, when supplied, must satisfy the conservative
//     paneTargetRE/length policy applied across the rest of the
//     surface so a stray quote or path-injection can't slip through
//     to tmux's argv;
//   - `format` / `filter` / `template` reject embedded newlines and
//     are bounded at 4 KiB each;
//   - `key_format` mirrors tmux's restrictive key-name character
//     class (alnum, dot, dash, underscore);
//   - `sort_order` is constrained to {time, name, size}.
//
// Unknown / missing target panes (and the headless "no server
// running" case) surface via the wrapped errs.ErrSessionNotFound
// which the JSON-RPC layer maps to CodeSessionNotFound.
func (t *Tools) chooseBuffer(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target    string `json:"target"`
		Format    string `json:"format"`
		Filter    string `json:"filter"`
		KeyFormat string `json:"key_format"`
		SortOrder string `json:"sort_order"`
		Template  string `json:"template"`
		NoPreview bool   `json:"no_preview"`
		Zoom      bool   `json:"zoom"`
		Reverse   bool   `json:"reverse"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}` here, and the zero values below all map to "no
	// flag emitted" in the controller. Mirrors choose_tree's null-args
	// branch.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("choose_buffer: %v", err)
		}
	}
	if args.Target != "" {
		if rerr := validatePaneTarget(args.Target); rerr != nil {
			return nil, invalidParams("choose_buffer: %s", rerr.Message)
		}
	}
	if rerr := validateChooseBufferFormat("format", args.Format); rerr != nil {
		return nil, rerr
	}
	if rerr := validateChooseBufferFormat("filter", args.Filter); rerr != nil {
		return nil, rerr
	}
	if rerr := validateChooseBufferFormat("template", args.Template); rerr != nil {
		return nil, rerr
	}
	if rerr := validateChooseBufferKeyFormat(args.KeyFormat); rerr != nil {
		return nil, rerr
	}
	if rerr := validateChooseBufferSortOrder(args.SortOrder); rerr != nil {
		return nil, rerr
	}

	// Resolve the pane target through the configured -session-prefix
	// the same way every other pane-targeting tool does, so a multi-
	// tenant deployment routes the choose-buffer call at the prefixed
	// tmux session the caller actually owns. Empty target flows
	// through unchanged so the "no -t" branch of ChooseBuffer's argv
	// builder keeps working.
	resolvedTarget := t.resolvePaneTarget(args.Target)
	if err := t.Ctl.ChooseBuffer(ctx,
		resolvedTarget,
		args.Format, args.Filter, args.KeyFormat, args.SortOrder, args.Template,
		args.NoPreview, args.Zoom, args.Reverse,
	); err != nil {
		return nil, internalError(fmt.Errorf("choose_buffer: %w", err))
	}
	return jsonBlock(map[string]any{"entered": true})
}

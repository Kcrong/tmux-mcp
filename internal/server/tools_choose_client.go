package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// maxChooseClientFormatLen caps every free-form string argument the
// choose_client tool forwards into tmux's format DSL: format, filter,
// key-format, sort-order, template. tmux happily parses long format
// strings (the DSL is recursive) but a realistic chooser configuration
// never exceeds a few hundred bytes — anything past 4 KiB is almost
// certainly a typo or hostile caller, and bounding here keeps the
// JSON-RPC frame size predictable.
const maxChooseClientFormatLen = 4096

// chooseClientToolDefs holds the JSON Schema for the choose_client
// tool. It is appended onto the main toolDefs slice from this file's
// init() so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
var chooseClientToolDefs = []map[string]any{
	{
		"name": "choose_client",
		"description": "Open an interactive client-chooser via `tmux choose-client " +
			"[-N] [-Z] [-r] [-t TARGET-PANE] [-F FORMAT] [-f FILTER] " +
			"[-K KEY-FORMAT] [-O SORT-ORDER] [TEMPLATE]`. tmux draws the " +
			"chooser inside `target` (or the active pane of the active " +
			"client when omitted) and lets the attached client pick which " +
			"connected tmux client to act on. The optional flags map " +
			"one-for-one onto tmux's: `no_preview` suppresses the preview " +
			"pane (-N), `zoom` zooms the chooser (-Z), `reverse` reverses " +
			"the sort order (-r). `format` / `filter` / `key_format` / " +
			"`sort_order` / `template` are forwarded verbatim when " +
			"non-empty so callers can re-skin the menu without rebuilding " +
			"tmux. Refuses with `-32000` (errs.ErrSessionNotFound) when " +
			"the target pane does not exist or no client is attached to " +
			"the server — the chooser is a UX affordance and cannot do " +
			"anything useful in either case.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Pane target (\"session\", \"session:window\", \"session:window.pane\", or `%N`).",
				},
				"format": map[string]any{
					"type":        "string",
					"maxLength":   maxChooseClientFormatLen,
					"description": "tmux format string for each menu line (-F).",
				},
				"filter": map[string]any{
					"type":        "string",
					"maxLength":   maxChooseClientFormatLen,
					"description": "tmux conditional that hides clients evaluating to false (-f).",
				},
				"key_format": map[string]any{
					"type":        "string",
					"maxLength":   maxChooseClientFormatLen,
					"description": "tmux format for the per-row hotkey label (-K).",
				},
				"sort_order": map[string]any{
					"type":        "string",
					"maxLength":   maxChooseClientFormatLen,
					"description": "Column to sort the menu by (-O), e.g. \"name\", \"size\", \"creation\".",
				},
				"template": map[string]any{
					"type":        "string",
					"maxLength":   maxChooseClientFormatLen,
					"description": "tmux command template run against the chosen client (TEMPLATE).",
				},
				"no_preview": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, suppress the preview pane (-N).",
				},
				"zoom": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, zoom the chooser pane (-Z).",
				},
				"reverse": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, reverse the sort order (-r).",
				},
			},
			// Every choose_client argument is optional — tmux defaults
			// each missing flag to its built-in client-chooser
			// behaviour. additionalProperties:false keeps the schema
			// strict so a typo like "key-format" (dash) fails fast at
			// the dispatcher rather than silently behaving like the
			// default-flag variant.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register choose_client onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from the single dispatcher case in tools.go) so
	// adding a sibling chooser tool later does not have to touch the
	// shared toolDefs literal.
	toolDefs = append(toolDefs, chooseClientToolDefs...)
}

// validateChooseClientFormat enforces the conservative shape on the
// free-form format/filter/key-format/sort-order/template arguments.
// All of them are optional, so the empty-string fast path is handled
// by the caller; this helper only sees non-empty values. Newlines are
// rejected because tmux's chooser would reflow them into the menu line
// and split the otherwise-single-row entry, breaking the schema's
// "one option per row" expectation.
func validateChooseClientFormat(field, value string) *rpcError {
	if value == "" {
		return nil
	}
	if len(value) > maxChooseClientFormatLen {
		return invalidParams("%s length %d exceeds %d", field, len(value), maxChooseClientFormatLen)
	}
	if strings.ContainsAny(value, "\n\r") {
		return invalidParams("%s: must not contain newlines", field)
	}
	return nil
}

// chooseClient drives tmuxctl.Controller.ChooseClient and serialises
// the result to the standard `{"content":[{"type":"text","text":"<json>"}]}`
// envelope MCP expects from a tools/call. The response shape is a flat
// `{"opened": true}` ack; the boundary deliberately does not echo the
// flag values back because choose-client is a fire-and-forget UX
// trigger — a follow-up call (e.g. list_clients to confirm the chooser
// fired) is one tool away if the agent wants confirmation.
//
// Up-front validation:
//   - `target` (when set) must satisfy paneTargetRE so a stray quote
//     or shell metachar cannot reach tmux;
//   - every free-form string argument is bounded against
//     maxChooseClientFormatLen and refused if it embeds a newline.
//
// Unknown targets and headless servers surface via the wrapped
// errs.ErrSessionNotFound the controller emits, which the JSON-RPC
// layer maps to CodeSessionNotFound (-32000).
func (t *Tools) chooseClient(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
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
	// json.Unmarshal on an empty payload is fine — every choose_client
	// argument is optional, so `arguments: {}` (or missing arguments
	// entirely) is a valid call.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("choose_client: %v", err)
		}
	}
	if args.Target != "" {
		if rerr := validatePaneTarget(args.Target); rerr != nil {
			return nil, invalidParams("choose_client: %s", rerr.Message)
		}
	}
	for _, f := range []struct {
		name, value string
	}{
		{"format", args.Format},
		{"filter", args.Filter},
		{"key_format", args.KeyFormat},
		{"sort_order", args.SortOrder},
		{"template", args.Template},
	} {
		if rerr := validateChooseClientFormat(f.name, f.value); rerr != nil {
			return nil, rerr
		}
	}
	if err := t.Ctl.ChooseClient(
		ctx,
		t.resolvePaneTarget(args.Target),
		args.Format, args.Filter, args.KeyFormat, args.SortOrder, args.Template,
		args.NoPreview, args.Zoom, args.Reverse,
	); err != nil {
		return nil, internalError(fmt.Errorf("choose_client: %w", err))
	}
	return jsonBlock(map[string]any{"opened": true})
}

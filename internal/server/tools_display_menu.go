package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// Length caps for every free-form display_menu argument. tmux's parser
// happily accepts much longer strings, but a realistic menu invocation
// stays well under any of these limits — anything past them is almost
// certainly a typo or hostile caller, and bounding here keeps the
// JSON-RPC frame size predictable.
const (
	maxDisplayMenuTitleLen       = 4096
	maxDisplayMenuItemNameLen    = 1024
	maxDisplayMenuItemKeyLen     = 64
	maxDisplayMenuItemCommandLen = 4096
	maxDisplayMenuStyleLen       = 256
	maxDisplayMenuPositionLen    = 32
	maxDisplayMenuItems          = 256
	maxDisplayMenuPaneTargetLen  = 256
)

// displayMenuPositionRE bounds the X/Y position arguments. tmux accepts
// either a row/column integer, one of the documented single-letter
// magic values (C, R, P, M, W, S), or a `#{...}` format. Restricting
// the schema to the union of those shapes keeps stray quotes / shell
// metachars from reaching tmux's argv. The format alternative is left
// permissive (any printable rune) so legitimate `#{popup_centre_x}`
// expressions still pass; the length cap above keeps this from being a
// blank check.
var displayMenuPositionRE = regexp.MustCompile(`^([0-9]+|[CRPMWS]|#\{[^}\n\r]*\})$`)

// displayMenuPaneTargetRE bounds the optional target_pane argument.
// tmux pane targets are always one of: a session name, a
// session:window, a session:window.pane, or a `%N` pane id. Reusing
// paneTargetRE would be ideal but that one requires the session half;
// our menu surface allows the bare `%N` pane id as well.
var displayMenuPaneTargetRE = regexp.MustCompile(`^([A-Za-z0-9_-]+(:[0-9]+(\.[0-9]+)?)?|%[0-9]+)$`)

// displayMenuItemKeyRE bounds the per-item key shortcut. tmux key
// definitions cover a fairly wide character set (letters, digits, the
// punctuation row, function keys) but never include whitespace, quotes,
// or shell metacharacters — so we anchor on the printable subset that
// tmux's bind-key parser accepts.
var displayMenuItemKeyRE = regexp.MustCompile(`^[A-Za-z0-9!@#$%^&*()\-_=+\[\]{};:'",.<>/?\\|` + "`" + `~]+$`)

// displayMenuToolDefs holds the JSON Schema for the display_menu tool.
// It is appended onto the main toolDefs slice via init() so the
// registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
var displayMenuToolDefs = []map[string]any{
	{
		"name": "display_menu",
		"description": "Render an interactive tmux menu via " +
			"`tmux display-menu [-O] [-b BORDER-LINES] [-c TARGET-CLIENT] " +
			"[-C STARTING-CHOICE] [-H SELECTED-STYLE] [-S BORDER-STYLE] " +
			"[-T TITLE] [-t TARGET-PANE] [-x POSITION] [-y POSITION] " +
			"name key command ...`. Each entry in `items` is one menu " +
			"row carrying a label, an optional single-key shortcut, and " +
			"an optional tmux command run on selection. The boundary " +
			"expands the array into the alternating positional triples " +
			"tmux's parser consumes. Refuses with `-32000` " +
			"(errs.ErrSessionNotFound) when the target client/pane is " +
			"missing or the server has no client to draw on.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target_pane": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayMenuPaneTargetLen,
					"description": "Pane target the menu's commands run against (`-t`).",
				},
				"target_client": map[string]any{
					"type":        "string",
					"maxLength":   maxClientNameLen,
					"description": "Client to draw the menu on (`-c`); TTY path like \"/dev/pts/3\".",
				},
				"title": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayMenuTitleLen,
					"description": "Optional menu title format (`-T`).",
				},
				"border_lines": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayMenuStyleLen,
					"description": "Border-line style passed to `-b` (single, double, heavy, simple, padded, none).",
				},
				"border_style": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayMenuStyleLen,
					"description": "tmux style spec for the border (`-S`), e.g. \"fg=red\".",
				},
				"selected_style": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayMenuStyleLen,
					"description": "tmux style spec for the highlighted row (`-H`).",
				},
				"starting_choice": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayMenuStyleLen,
					"description": "Index/label of the row pre-selected when the menu opens (`-C`).",
				},
				"x": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayMenuPositionLen,
					"description": "Horizontal position (`-x`): integer column, magic letter (C/R/P/M/W), or `#{...}` format.",
				},
				"y": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayMenuPositionLen,
					"description": "Vertical position (`-y`): integer row, magic letter (C/P/M/W/S), or `#{...}` format.",
				},
				"no_callbacks": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, pass `-O` so the menu does not close on mouse release without a selection.",
				},
				"items": map[string]any{
					"type":        "array",
					"minItems":    1,
					"maxItems":    maxDisplayMenuItems,
					"description": "Menu rows. Order of the array is the order rendered top-to-bottom.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name": map[string]any{
								"type":        "string",
								"minLength":   1,
								"maxLength":   maxDisplayMenuItemNameLen,
								"description": "Row label (format-evaluated by tmux). Required and non-empty.",
							},
							"key": map[string]any{
								"type":        "string",
								"maxLength":   maxDisplayMenuItemKeyLen,
								"description": "Optional single-key shortcut shown in brackets next to the row.",
							},
							"command": map[string]any{
								"type":        "string",
								"maxLength":   maxDisplayMenuItemCommandLen,
								"description": "Optional tmux command run when the row is chosen.",
							},
						},
						"required":             []string{"name"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"items"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register display_menu onto the main toolDefs slice. Keeping the
	// init() registration in this file means adding a sibling menu tool
	// later does not have to touch the shared toolDefs literal.
	toolDefs = append(toolDefs, displayMenuToolDefs...)
}

// validateDisplayMenuPaneTarget enforces the conservative shape on the
// optional target_pane argument. Empty is allowed (tmux falls back to
// the active pane of the active client); a non-empty value must
// satisfy displayMenuPaneTargetRE so a stray quote or metachar cannot
// reach tmux.
func validateDisplayMenuPaneTarget(target string) *rpcError {
	if target == "" {
		return nil
	}
	if len(target) > maxDisplayMenuPaneTargetLen {
		return invalidParams("display_menu: target_pane length %d exceeds %d",
			len(target), maxDisplayMenuPaneTargetLen)
	}
	if !displayMenuPaneTargetRE.MatchString(target) {
		return invalidParams("display_menu: target_pane %q must match %s",
			target, displayMenuPaneTargetRE.String())
	}
	return nil
}

// validateDisplayMenuStyleField bounds title/border_style/selected_style/
// starting_choice/border_lines: all are optional, all must be free of
// embedded newlines (tmux would reflow the menu line and break the
// schema's "single row per option" contract), and all are length-capped
// to the per-field maximum.
func validateDisplayMenuStyleField(field, value string, max int) *rpcError {
	if value == "" {
		return nil
	}
	if len(value) > max {
		return invalidParams("display_menu: %s length %d exceeds %d", field, len(value), max)
	}
	if strings.ContainsAny(value, "\n\r") {
		return invalidParams("display_menu: %s must not contain newlines", field)
	}
	return nil
}

// validateDisplayMenuTitle is a thin wrapper that adds a NUL byte
// rejection on top of the shared style-field validator. tmux strings
// must not embed NUL because tmux passes them to its own format
// engine which is C-string typed; the rest of the style fields go
// through the same gate so the title check is just a specialisation.
func validateDisplayMenuTitle(title string) *rpcError {
	if rerr := validateDisplayMenuStyleField("title", title, maxDisplayMenuTitleLen); rerr != nil {
		return rerr
	}
	if strings.ContainsRune(title, 0) {
		return invalidParams("display_menu: title must not contain NUL bytes")
	}
	return nil
}

// validateDisplayMenuPosition checks an X/Y argument against the union
// of integer / magic-letter / format shapes the tmux man page allows.
// Empty is OK (tmux defaults the position based on the menu source).
func validateDisplayMenuPosition(field, value string) *rpcError {
	if value == "" {
		return nil
	}
	if len(value) > maxDisplayMenuPositionLen {
		return invalidParams("display_menu: %s length %d exceeds %d",
			field, len(value), maxDisplayMenuPositionLen)
	}
	if !displayMenuPositionRE.MatchString(value) {
		return invalidParams("display_menu: %s %q must match %s",
			field, value, displayMenuPositionRE.String())
	}
	return nil
}

// validateDisplayMenuItem applies the full per-row policy: name is
// required, key (when set) must satisfy displayMenuItemKeyRE, command
// (when set) is length-capped and forbidden from embedding newlines.
// The schema-side `additionalProperties:false` catches typoed fields,
// so this function only has to look at the canonical three.
func validateDisplayMenuItem(idx int, it tmuxctl.DisplayMenuItem) *rpcError {
	if it.Name == "" {
		return invalidParams("display_menu: items[%d].name is required", idx)
	}
	if len(it.Name) > maxDisplayMenuItemNameLen {
		return invalidParams("display_menu: items[%d].name length %d exceeds %d",
			idx, len(it.Name), maxDisplayMenuItemNameLen)
	}
	if strings.ContainsAny(it.Name, "\n\r") {
		return invalidParams("display_menu: items[%d].name must not contain newlines", idx)
	}
	if it.Key != "" {
		if len(it.Key) > maxDisplayMenuItemKeyLen {
			return invalidParams("display_menu: items[%d].key length %d exceeds %d",
				idx, len(it.Key), maxDisplayMenuItemKeyLen)
		}
		if !displayMenuItemKeyRE.MatchString(it.Key) {
			return invalidParams("display_menu: items[%d].key %q has unsupported characters",
				idx, it.Key)
		}
	}
	if it.Command != "" {
		if len(it.Command) > maxDisplayMenuItemCommandLen {
			return invalidParams("display_menu: items[%d].command length %d exceeds %d",
				idx, len(it.Command), maxDisplayMenuItemCommandLen)
		}
		if strings.ContainsAny(it.Command, "\n\r") {
			return invalidParams("display_menu: items[%d].command must not contain newlines", idx)
		}
	}
	return nil
}

// displayMenu drives [tmuxctl.Controller.DisplayMenu] and serialises
// the result to the standard `{"content":[{"type":"text","text":"<json>"}]}`
// envelope MCP expects from a tools/call. The response is a flat
// `{"displayed": true}` ack — the menu is a fire-and-forget UX trigger
// and the boundary deliberately does not echo the items back.
//
// Up-front validation:
//   - `target_pane` (when set) must satisfy displayMenuPaneTargetRE;
//   - `target_client` (when set) goes through the shared validateClientRef;
//   - title/border_*/selected_style/starting_choice are bounded
//     against their per-field length cap and refused if they embed a
//     newline;
//   - x/y must look like an integer / magic letter / `#{...}` format;
//   - items must be non-empty (the schema enforces minItems:1 too)
//     and every entry must carry a non-empty name.
//
// Unknown clients/panes and headless servers surface via the wrapped
// errs.ErrSessionNotFound the controller emits, which the JSON-RPC
// layer maps to CodeSessionNotFound (-32000).
func (t *Tools) displayMenu(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		TargetPane     string `json:"target_pane"`
		TargetClient   string `json:"target_client"`
		Title          string `json:"title"`
		BorderLines    string `json:"border_lines"`
		BorderStyle    string `json:"border_style"`
		SelectedStyle  string `json:"selected_style"`
		StartingChoice string `json:"starting_choice"`
		X              string `json:"x"`
		Y              string `json:"y"`
		NoCallbacks    bool   `json:"no_callbacks"`
		Items          []struct {
			Name    string `json:"name"`
			Key     string `json:"key"`
			Command string `json:"command"`
		} `json:"items"`
	}
	if len(raw) == 0 {
		return nil, invalidParams("display_menu: items is required")
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("display_menu: %v", err)
	}
	if len(args.Items) == 0 {
		return nil, invalidParams("display_menu: items must contain at least one entry")
	}
	if len(args.Items) > maxDisplayMenuItems {
		return nil, invalidParams("display_menu: items length %d exceeds %d",
			len(args.Items), maxDisplayMenuItems)
	}
	if rerr := validateDisplayMenuPaneTarget(args.TargetPane); rerr != nil {
		return nil, rerr
	}
	if rerr := validateClientRef(args.TargetClient); rerr != nil {
		// validateClientRef phrases its errors with the show_messages
		// tool name; rewrite to keep the surface consistent for
		// display_menu callers.
		return nil, invalidParams("display_menu: %s",
			strings.TrimPrefix(rerr.Message, "show_messages: "))
	}
	if rerr := validateDisplayMenuTitle(args.Title); rerr != nil {
		return nil, rerr
	}
	for _, f := range []struct {
		name, value string
		max         int
	}{
		{"border_lines", args.BorderLines, maxDisplayMenuStyleLen},
		{"border_style", args.BorderStyle, maxDisplayMenuStyleLen},
		{"selected_style", args.SelectedStyle, maxDisplayMenuStyleLen},
		{"starting_choice", args.StartingChoice, maxDisplayMenuStyleLen},
	} {
		if rerr := validateDisplayMenuStyleField(f.name, f.value, f.max); rerr != nil {
			return nil, rerr
		}
	}
	if rerr := validateDisplayMenuPosition("x", args.X); rerr != nil {
		return nil, rerr
	}
	if rerr := validateDisplayMenuPosition("y", args.Y); rerr != nil {
		return nil, rerr
	}

	items := make([]tmuxctl.DisplayMenuItem, 0, len(args.Items))
	for i, raw := range args.Items {
		it := tmuxctl.DisplayMenuItem{
			Name:    raw.Name,
			Key:     raw.Key,
			Command: raw.Command,
		}
		if rerr := validateDisplayMenuItem(i, it); rerr != nil {
			return nil, rerr
		}
		items = append(items, it)
	}

	if err := t.Ctl.DisplayMenu(ctx, tmuxctl.DisplayMenuOpts{
		TargetPane:     t.resolvePaneTarget(args.TargetPane),
		TargetClient:   args.TargetClient,
		Title:          args.Title,
		BorderLines:    args.BorderLines,
		BorderStyle:    args.BorderStyle,
		SelectedStyle:  args.SelectedStyle,
		StartingChoice: args.StartingChoice,
		X:              args.X,
		Y:              args.Y,
		NoCallbacks:    args.NoCallbacks,
		Items:          items,
	}); err != nil {
		return nil, internalError(fmt.Errorf("display_menu: %w", err))
	}
	return jsonBlock(map[string]any{"displayed": true})
}

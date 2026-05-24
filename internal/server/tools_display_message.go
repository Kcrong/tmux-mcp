package server

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// maxDisplayFormatLen caps the format string for display_message. tmux
// happily accepts long formats (the DSL is recursive), but a realistic
// introspection call rarely exceeds a few hundred bytes — anything past
// 4 KiB is almost certainly a typo or hostile caller, and bounding
// here keeps the JSON-RPC frame size predictable.
const maxDisplayFormatLen = 4096

// displayPaneRE accepts the standalone pane forms display_message will
// splice into a tmux target after the `.` separator: a numeric pane
// index (e.g. "1") or a tmux internal pane id (e.g. "%5"). We
// deliberately do not accept the full `session:window.pane` shape
// here — that is what the separate `session` and `window` arguments
// are for.
var displayPaneRE = regexp.MustCompile(`^([0-9]+|%[0-9]+)$`)

// validateDisplayPane enforces the conservative shape on
// display_message's optional `pane` argument. The schema marks the
// field optional, so the empty-string fast path is handled by the
// caller; this helper only sees non-empty values.
func validateDisplayPane(pane string) *rpcError {
	if len(pane) > maxSessionNameLen {
		return invalidParams("pane length %d out of range [1..%d]", len(pane), maxSessionNameLen)
	}
	if !displayPaneRE.MatchString(pane) {
		return invalidParams("pane %q must match %s", pane, displayPaneRE.String())
	}
	return nil
}

// displayMessageToolDefs holds the JSON Schema for the display_message
// tool. It is appended onto the main toolDefs slice via the package
// init() in this file so the registration site stays close to the
// handler — the dispatcher in tools.go only needs the single name →
// handler entry.
var displayMessageToolDefs = []map[string]any{
	{
		"name": "display_message",
		"description": "Evaluate a tmux format string via `tmux display-message -p` and return the " +
			"resolved single-line value. The canonical introspection escape hatch for any " +
			"`#{...}` variable that does not yet have a dedicated tool — pane titles, window " +
			"options, server uptime, etc. Optional `session` / `window` / `pane` combine into " +
			"a tmux target (`<session>`, `<session>:<window>`, `<session>:<window>.<pane>`); " +
			"omit them all to evaluate the format against tmux's current/global context.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"format": map[string]any{
					"type":        "string",
					"description": "tmux format string (e.g. `#{session_name} #{pane_current_path}`); must not contain newlines.",
				},
				"session": map[string]any{
					"type":        "string",
					"description": "Optional session id; len 1-64, [A-Za-z0-9_-].",
				},
				"window": map[string]any{
					"type":        "string",
					"description": "Optional window name (1-64, [A-Za-z0-9_-]) or numeric index (\\d+).",
				},
				"pane": map[string]any{
					"type":        "string",
					"description": "Optional pane index (\\d+) or tmux `%N` pane id.",
				},
			},
			"required":             []string{"format"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register display_message onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from a single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, displayMessageToolDefs...)
}

// displayMessage drives tmuxctl.Controller.DisplayMessage and
// serialises the result to the standard
// `{"content":[{"type":"text","text":"<json>"}]}` envelope MCP expects
// for tools/call. The output shape is intentionally a flat object
// keyed by "value" so future additions (e.g. a "format" echo) do not
// break callers that only read the resolved string.
//
// Up-front validation:
//   - format required and bounded (no newlines, length cap) before any
//     tmux call runs;
//   - session / window / pane each optional, but when present each must
//     match the same conservative regex/length policy applied
//     elsewhere on the boundary so a stray quote or shell metachar
//     cannot slip through to tmux.
//
// Unknown targets surface via the wrapped errs.ErrSessionNotFound
// sentinel, which the JSON-RPC layer maps to CodeSessionNotFound
// (-32000).
func (t *Tools) displayMessage(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Format  string `json:"format"`
		Session string `json:"session"`
		Window  string `json:"window"`
		Pane    string `json:"pane"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("display_message: %v", err)
	}
	if args.Format == "" {
		return nil, invalidParams("format: required")
	}
	if strings.ContainsAny(args.Format, "\n\r") {
		return nil, invalidParams("format: must not contain newlines")
	}
	if len(args.Format) > maxDisplayFormatLen {
		return nil, invalidParams("format length %d exceeds %d", len(args.Format), maxDisplayFormatLen)
	}
	if args.Session != "" {
		if rerr := validateSessionRef(args.Session); rerr != nil {
			return nil, rerr
		}
	}
	if args.Window != "" {
		if rerr := validateWindowTarget(args.Window); rerr != nil {
			return nil, rerr
		}
	}
	if args.Pane != "" {
		if rerr := validateDisplayPane(args.Pane); rerr != nil {
			return nil, rerr
		}
	}
	value, err := t.Ctl.DisplayMessage(ctx, args.Format, args.Session, args.Window, args.Pane)
	if err != nil {
		return nil, internalError(err)
	}
	return jsonBlock(map[string]any{"value": value})
}

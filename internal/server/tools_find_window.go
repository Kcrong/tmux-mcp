package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// findWindowToolDefs holds the JSON Schema for the find_window tool.
// The block is appended onto the main toolDefs slice from this file's
// init() so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
//
// find_window is the inspection counterpart to list_windows: rather
// than enumerating every window and forcing the agent to filter
// client-side, it pushes the predicate into tmux so only the matching
// rows come back. Useful when an agent has tens of long-lived sessions
// and wants to find "the build window in any of them" without
// streaming the full list.
var findWindowToolDefs = []map[string]any{
	{
		"name": "find_window",
		"description": "Search for windows whose name, pane title, or visible content matches `match`. " +
			"By default the search runs across all three scopes (the same `-CNT` default tmux's " +
			"`find-window` uses); set `name_only`, `title_only`, or `content_only` to restrict, or " +
			"combine them to compose a union (any selected scope hits is enough). `regex` (default " +
			"false) flips matching from fnmatch globbing to a regular expression. `target`, when " +
			"present, restricts the search to one session (`-t <session>`); otherwise every window " +
			"on the server is considered (`-a`). Returns an array of `{session, window_index, " +
			"window_name}` rows — empty (not null) when nothing matched.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"match": map[string]any{
					"type":        "string",
					"minLength":   1,
					"description": "Pattern to search for. fnmatch by default (substring); regex when `regex=true`.",
				},
				"regex": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, treat `match` as a Go-style regex (`-r`). Default is fnmatch substring.",
				},
				"name_only": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "Restrict matching to the window name (`-N`). Combine with other `*_only` flags to union scopes.",
				},
				"title_only": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "Restrict matching to the window's pane title (`-T`).",
				},
				"content_only": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "Restrict matching to visible pane content (`-C`).",
				},
				"target": map[string]any{
					"type":        "string",
					"maxLength":   maxSessionNameLen,
					"description": "Optional session name to scope the search; len 1-64, [A-Za-z0-9_-]. Omit to search every session.",
				},
			},
			"required":             []string{"match"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register find_window onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, findWindowToolDefs...)
}

// findWindow drives tmuxctl.Controller.FindWindow. Up-front it
// validates the optional target session reference and the required
// match string, then asks tmux to enumerate matching windows. The
// response shape mirrors list_windows' "flat object keyed by an array"
// convention so a future filter (e.g. `pane_count`, `active_only`)
// can land without breaking callers that iterate the result.
//
// `match` is required and rejected as -32602 when empty so a stray
// "find every window" call cannot mask a typo. `target`, when
// supplied, must satisfy the same regex/length policy as every other
// session reference; an unknown target surfaces as -32000 via
// errs.ErrSessionNotFound. The `_only` flags compose as a union
// matching the controller-side semantics.
func (t *Tools) findWindow(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Match       string `json:"match"`
		Regex       bool   `json:"regex"`
		NameOnly    bool   `json:"name_only"`
		TitleOnly   bool   `json:"title_only"`
		ContentOnly bool   `json:"content_only"`
		Target      string `json:"target"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("find_window: %v", err)
	}
	if args.Match == "" {
		// minLength on the schema already enforces this, but the explicit
		// runtime check guards against a hand-built tools/call that
		// bypasses schema validation (every other tool in this surface
		// has the same belt-and-braces check on required strings).
		return nil, invalidParams("match required")
	}
	if args.Target != "" {
		if rerr := validateSessionRef(args.Target); rerr != nil {
			return nil, rerr
		}
	}
	// Apply -session-prefix when the caller scoped the search to a
	// single session so we hit the actual tmux session the rest of the
	// surface addresses. Empty target preserves the unscoped (-a) path.
	matches, err := t.Ctl.FindWindow(ctx, args.Match, tmuxctl.FindWindowOpts{
		NameOnly:    args.NameOnly,
		TitleOnly:   args.TitleOnly,
		ContentOnly: args.ContentOnly,
		Regex:       args.Regex,
		Target:      t.resolveSessionRef(args.Target),
	})
	if err != nil {
		return nil, internalError(fmt.Errorf("find_window: %w", err))
	}
	out := make([]map[string]any, 0, len(matches))
	for _, m := range matches {
		// Strip the -session-prefix on the way out so a deployment that
		// uses prefixing never leaks the prefixed identity to the
		// caller. stripSessionPrefix is a no-op when SessionPrefix is
		// empty, so the unprefixed deployment sees the raw tmux value.
		out = append(out, map[string]any{
			"session":      t.stripSessionPrefix(m.Session),
			"window_index": m.WindowIndex,
			"window_name":  m.WindowName,
		})
	}
	return jsonBlock(map[string]any{"matches": out})
}

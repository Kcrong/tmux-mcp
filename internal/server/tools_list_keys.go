package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// maxKeyTableLen / maxKeyPrefixLen cap the up-front length checks
// applied to the optional `key_table` and `prefix` arguments so a
// hostile or buggy caller cannot blow up tmux's argv with a megabyte
// payload before we even reach the boundary. tmux's own table names
// are short ("prefix", "root", "copy-mode", "copy-mode-vi"); 64 chars
// is generous while staying well under any reasonable shell-arg cap.
const (
	maxKeyTableLen  = 64
	maxKeyPrefixLen = 64
)

// keyTableRE accepts the same conservative shape used elsewhere on the
// boundary plus a literal '-' (tmux's built-in tables include
// "copy-mode" and "copy-mode-vi"): alnum, underscore, dash. Anything
// else (whitespace, shell metachars, '/', '..') is rejected up-front
// so a stray quote can never reach tmux's argv. The table name is a
// single token tmux looks up in its key-bindings table — there is no
// reason for it to contain anything more exotic.
var keyTableRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// listKeysToolDefs holds the JSON Schema for the list_keys tool. It is
// appended onto the main toolDefs slice via the package init() in this
// file so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
var listKeysToolDefs = []map[string]any{
	{
		"name": "list_keys",
		"description": "Enumerate the key bindings on this controller's tmux server via " +
			"`tmux list-keys`. Useful for an agent that needs to introspect what " +
			"a key chord does before sending it through `send_keys`, or to confirm " +
			"a custom binding installed by an init script took effect. Pass " +
			"`key_table` to scope to a single keymap (`-T TABLE`); pass " +
			"`notes_only=true` to restrict the response to bindings annotated with " +
			"a `bind-key -N` note (the third column then carries the note text " +
			"instead of the command); pass `prefix` to forward `-P PREFIX`, which " +
			"tmux uses to prefix the rendered key chord in notes-only output. " +
			"Each entry carries `{table, key, command}`.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key_table": map[string]any{
					"type":        "string",
					"maxLength":   maxKeyTableLen,
					"description": "Optional keymap name (e.g. \"prefix\", \"root\", \"copy-mode\"); maps to `-T TABLE`. Omit to list every table.",
				},
				"notes_only": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, restrict the listing to bindings annotated with a `bind-key -N` note (`-N`).",
				},
				"prefix": map[string]any{
					"type":        "string",
					"maxLength":   maxKeyPrefixLen,
					"description": "Optional render-time prefix prepended to every rendered key chord (`-P PREFIX`); only meaningful in notes-only mode.",
				},
			},
			// Locking additionalProperties keeps the schema strict so
			// an agent that misnames a field (e.g. "table" instead of
			// "key_table") gets a fast schema-shaped rejection rather
			// than a silent no-op.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register list_keys onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go and the
	// readonly.go allowlist entry) and avoids touching the shared
	// toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, listKeysToolDefs...)
}

// validateKeyTable enforces the conservative key-table policy for the
// optional `key_table` argument. Empty is allowed (the controller falls
// back to every table); a non-empty value must satisfy the regex/length
// policy so a stray quote or path-injection attempt can't slip through
// to tmux's argv.
func validateKeyTable(name string) *rpcError {
	if name == "" {
		return nil
	}
	if len(name) > maxKeyTableLen {
		return invalidParams("list_keys: key_table length %d out of range [1..%d]", len(name), maxKeyTableLen)
	}
	if !keyTableRE.MatchString(name) {
		return invalidParams("list_keys: key_table %q must match %s", name, keyTableRE.String())
	}
	return nil
}

// validateKeyPrefix bounds the optional `prefix` argument. tmux uses
// the prefix as a verbatim render-time decoration so it can contain
// arbitrary characters in principle — we only cap the length to keep
// the JSON-RPC payload bounded and reject the empty-but-present case
// (an empty string is functionally equivalent to omitting the field;
// rejecting the explicit empty value here keeps the schema's "omit to
// disable" contract uniform).
func validateKeyPrefix(prefix string) *rpcError {
	if prefix == "" {
		return nil
	}
	if len(prefix) > maxKeyPrefixLen {
		return invalidParams("list_keys: prefix length %d out of range [1..%d]", len(prefix), maxKeyPrefixLen)
	}
	return nil
}

// listKeys drives tmuxctl.Controller.ListKeys and serialises the
// result to the standard `{"content":[{"type":"text","text":"<json>"}]}`
// envelope MCP expects from a tools/call. The response shape is a flat
// object keyed by "keys" so a future filter (e.g. a "command_match"
// regex knob, or an "include_root" flag) can be added without breaking
// callers that iterate the list.
//
// Empty stdout (no bindings match the requested filters) returns
// `{"keys": []}` cleanly rather than an error so the JSON-RPC layer
// doesn't have to substring-match tmux stderr to tell the cases apart.
// A truly bad invocation (unknown key table, malformed prefix) is
// caught up-front by the validators below; anything that slips past
// surfaces via internalError.
func (t *Tools) listKeys(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		KeyTable  string `json:"key_table"`
		NotesOnly bool   `json:"notes_only"`
		Prefix    string `json:"prefix"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}` here, and the zero values mean "every table,
	// default rendering, no prefix decoration" which is exactly the
	// `tmux list-keys` no-arg behaviour.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("list_keys: %v", err)
		}
	}
	if rerr := validateKeyTable(args.KeyTable); rerr != nil {
		return nil, rerr
	}
	if rerr := validateKeyPrefix(args.Prefix); rerr != nil {
		return nil, rerr
	}
	keys, err := t.Ctl.ListKeys(ctx, tmuxctl.ListKeysOpts{
		Table:     args.KeyTable,
		NotesOnly: args.NotesOnly,
		Prefix:    args.Prefix,
	})
	if err != nil {
		return nil, internalError(fmt.Errorf("list_keys: %w", err))
	}
	out := make([]map[string]any, 0, len(keys))
	for _, kb := range keys {
		out = append(out, map[string]any{
			"table":   kb.Table,
			"key":     kb.Key,
			"command": kb.Command,
		})
	}
	return jsonBlock(map[string]any{"keys": out})
}

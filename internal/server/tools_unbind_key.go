package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// maxUnbindKeyLen caps the optional `key` argument so a hostile or
// buggy caller cannot blow up tmux's argv with a megabyte payload
// before the boundary even runs. tmux keysyms are short (e.g. "C-a",
// "Up", "M-{"); 256 bytes is generous while staying well under any
// reasonable shell-arg cap.
const maxUnbindKeyLen = 256

// unbindKeyToolDefs holds the JSON Schema for the unbind_key tool. It
// is appended onto the main toolDefs slice from this file's init() so
// the registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
//
// All three fields are schema-optional because the {key xor all=true}
// constraint is not expressible in plain JSON Schema; the handler
// enforces it explicitly with a CodeInvalidParams error.
//
// Locking additionalProperties keeps the schema strict so an agent
// that misnames a field (e.g. "table" instead of "key_table") gets a
// fast schema-shaped rejection rather than a silent no-op.
var unbindKeyToolDefs = []map[string]any{
	{
		"name": "unbind_key",
		"description": "Remove a tmux key binding via `tmux unbind-key [-a] [-T TABLE] [KEY]`. " +
			"Sister of `bind_key` and `list_keys`: pass `key` to remove a single chord, " +
			"or set `all=true` to wipe every binding in the targeted table (`-a`). " +
			"`key_table` scopes the removal to a specific keymap (`-T TABLE`); omit it " +
			"to use tmux's default table for the operation. Exactly one of {`key` set, " +
			"`all=true`} is required — both empty would silently no-op, both set " +
			"contradicts each other. Idempotent: removing a chord that was never bound " +
			"(or was already unbound) succeeds without an error.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{
					"type":        "string",
					"maxLength":   maxUnbindKeyLen,
					"description": "Key chord to remove (e.g. \"C-a\", \"F12\", \"M-{\"). Mutually exclusive with `all=true`.",
				},
				"key_table": map[string]any{
					"type":        "string",
					"maxLength":   maxKeyTableLen,
					"description": "Optional keymap name (e.g. \"prefix\", \"root\", \"copy-mode\"); maps to `-T TABLE`. Omit to use tmux's default table.",
				},
				"all": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, remove every binding in the targeted table (`-a`). Mutually exclusive with `key`.",
				},
			},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register unbind_key onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, unbindKeyToolDefs...)
}

// validateUnbindKey enforces the per-field policy on `key`. Empty is
// allowed (the {all=true} shape uses an empty key); a non-empty value
// must stay within the length cap and carry no NUL or ASCII control
// bytes. We deliberately allow DEL (0x7f) and the high range so tmux
// keysyms like "Up", "C-Space", or genuinely high-byte keysyms are not
// gratuitously refused at the boundary.
func validateUnbindKey(key string) *rpcError {
	if key == "" {
		return nil
	}
	if len(key) > maxUnbindKeyLen {
		return invalidParams("unbind_key: key length %d out of range [1..%d]", len(key), maxUnbindKeyLen)
	}
	for i := 0; i < len(key); i++ {
		// Reject NUL (0x00) and the rest of the C0 control range
		// (0x01-0x1f). DEL (0x7f) is left alone — tmux keysyms can in
		// principle include it, and the boundary's job is to keep tmux's
		// argv bounded, not to second-guess every legal keysym.
		if key[i] == 0 || (key[i] > 0 && key[i] < 0x20) {
			return invalidParams("unbind_key: key contains a NUL or control byte at offset %d", i)
		}
	}
	return nil
}

// validateUnbindKeyTable enforces the same conservative regex/length
// policy on `key_table` that list_keys uses. Empty is allowed; a
// non-empty value must satisfy keyTableRE so a stray quote or
// path-injection attempt cannot reach tmux's argv.
func validateUnbindKeyTable(name string) *rpcError {
	if name == "" {
		return nil
	}
	if len(name) > maxKeyTableLen {
		return invalidParams("unbind_key: key_table length %d out of range [1..%d]", len(name), maxKeyTableLen)
	}
	if !keyTableRE.MatchString(name) {
		return invalidParams("unbind_key: key_table %q must match %s", name, keyTableRE.String())
	}
	return nil
}

// unbindKey drives tmuxctl.Controller.UnbindKey. The handler enforces
// the {key xor all=true} contract explicitly because plain JSON Schema
// cannot express it — both fields are schema-optional and the mutual-
// exclusion check fires up front so callers see a clean -32602 instead
// of tmux's version-dependent stderr.
//
// Response is a small JSON ack (`{"unbound": true}`) so callers that
// chain unbind_key with another mutation can branch on a stable shape
// rather than parse a free-form status string. Idempotent removals
// (a double-unbind, or unbinding a chord that was never bound) return
// the same ack — tmux itself does not distinguish those cases, and
// surfacing the difference would push every caller to write a
// "did anything actually change?" branch they do not need.
func (t *Tools) unbindKey(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Key      string `json:"key"`
		KeyTable string `json:"key_table"`
		All      bool   `json:"all"`
	}
	// Allow an absent / empty arguments object so the typed validation
	// below produces the canonical "must set either key or all=true"
	// error rather than a json.Unmarshal complaint.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("unbind_key: %v", err)
		}
	}
	if rerr := validateUnbindKey(args.Key); rerr != nil {
		return nil, rerr
	}
	if rerr := validateUnbindKeyTable(args.KeyTable); rerr != nil {
		return nil, rerr
	}
	switch {
	case args.All && args.Key != "":
		return nil, invalidParams("unbind_key: all=true cannot be combined with a non-empty key")
	case !args.All && args.Key == "":
		return nil, invalidParams("unbind_key: must set either key or all=true")
	}
	if err := t.Ctl.UnbindKey(ctx, args.Key, args.KeyTable, args.All); err != nil {
		return nil, internalError(fmt.Errorf("unbind_key: %w", err))
	}
	return jsonBlock(map[string]any{"unbound": true})
}

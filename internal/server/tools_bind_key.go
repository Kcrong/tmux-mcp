package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// maxBindKeyKeyLen / maxBindKeyCommandLen cap the up-front length
// checks applied to the required `key` and `command` arguments so a
// hostile or buggy caller cannot blow up tmux's argv with a megabyte
// payload before we even reach the boundary. tmux's keysym strings are
// short by construction ("C-Space", "M-x", "Up"); 256 bytes is generous
// while staying well under any reasonable argv cap. Commands legitimately
// run longer (a tmux command may chain multiple verbs via `\;`), so 4
// KiB matches the same ceiling other write-side tools (e.g. set_buffer
// handler comments) describe as "ample for any realistic caller".
const (
	maxBindKeyKeyLen     = 256
	maxBindKeyCommandLen = 4096
)

// bindKeyToolDefs holds the JSON Schema for the bind_key tool. It is
// appended onto the main toolDefs slice via this file's init() so the
// registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
//
// bind_key writes server-wide state (the key map lives on the tmux
// server, not on a session), so this tool is deliberately not session-
// scoped: there is no `session` field in the schema and SessionPrefix
// does not apply.
var bindKeyToolDefs = []map[string]any{
	{
		"name": "bind_key",
		"description": "Register a tmux key binding via `tmux bind-key [-T TABLE] [-r] KEY COMMAND`. " +
			"The write counterpart of `list_keys` (which reads the same key map back). " +
			"`key` is the literal keysym string tmux's parser knows (e.g. \"C-Space\", " +
			"\"M-x\", \"Up\"); `command` is the entire tmux command line that should fire " +
			"when the chord is pressed (do NOT split on whitespace before passing it in — " +
			"tmux parses the command server-side via `command_parse_string`, so the whole " +
			"string travels through as a single argv element). Pass `key_table` to scope " +
			"the binding to a single keymap (`-T TABLE`); omit to land on tmux's default " +
			"table (\"prefix\" on tmux 3.4). Set `repeatable=true` to add `-r`, which lets " +
			"tmux fire the binding repeatedly while the prefix table stays armed (used by " +
			"the built-in resize/select pane chords). On success the response carries " +
			"`{\"bound\": true, \"key\": \"<key>\", \"table\": \"<table>\"}`.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   maxBindKeyKeyLen,
					"description": "Keysym string tmux's parser recognises (e.g. \"C-Space\", \"M-x\", \"Up\").",
				},
				"command": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   maxBindKeyCommandLen,
					"description": "Entire tmux command line to bind to the chord; passed verbatim as a single argv element.",
				},
				"key_table": map[string]any{
					"type":        "string",
					"maxLength":   maxKeyTableLen,
					"description": "Optional keymap name (e.g. \"prefix\", \"root\", \"copy-mode\", \"copy-mode-vi\"); maps to `-T TABLE`. Omit to land in tmux's default table.",
				},
				"repeatable": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, add `-r` so the binding can repeat while the prefix table stays armed.",
				},
			},
			"required":             []string{"key", "command"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register bind_key onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, bindKeyToolDefs...)
}

// validateBindKeyChord enforces the conservative key-chord policy on
// the required `key` argument: non-empty, ≤256 bytes, no NUL or other
// ASCII control bytes (DEL is allowed because tmux uses literal keysym
// strings — there is no legitimate caller that needs `\x00` in there,
// and a NUL byte would hard-stop most argv-handling code below us).
//
// The validator runs ahead of the boundary call so a malformed `key`
// is rejected with CodeInvalidParams (-32602) before any tmux process
// is spawned.
func validateBindKeyChord(key string) *rpcError {
	if key == "" {
		return invalidParams("bind_key: key required")
	}
	if len(key) > maxBindKeyKeyLen {
		return invalidParams("bind_key: key length %d out of range [1..%d]", len(key), maxBindKeyKeyLen)
	}
	for i := 0; i < len(key); i++ {
		// Reject ASCII control bytes (NUL through US, byte 0x1F). DEL
		// (0x7F) is intentionally kept allowed: tmux's keysym table
		// uses a few names that map to high-bit characters, and
		// excluding DEL here would block legitimate keysyms with no
		// safety upside.
		if b := key[i]; b < 0x20 {
			return invalidParams("bind_key: key contains control byte at index %d", i)
		}
	}
	return nil
}

// validateBindKeyCommand enforces the same shape policy on the
// required `command` argument: non-empty, ≤4096 bytes, no NUL or
// non-tab ASCII control bytes. tmux command strings legitimately
// contain tab characters (as separators inside string-quoted
// arguments) but never NUL or other control bytes — those would
// indicate an encoding bug at the caller.
func validateBindKeyCommand(cmd string) *rpcError {
	if cmd == "" {
		return invalidParams("bind_key: command required")
	}
	if len(cmd) > maxBindKeyCommandLen {
		return invalidParams("bind_key: command length %d out of range [1..%d]", len(cmd), maxBindKeyCommandLen)
	}
	for i := 0; i < len(cmd); i++ {
		b := cmd[i]
		// Permit horizontal tab (0x09); reject every other control byte
		// up to (but excluding) space. tmux commands contain enough
		// quote/escape syntax that a stray control byte is almost
		// certainly an encoding bug rather than an intentional payload.
		if b < 0x20 && b != '\t' {
			return invalidParams("bind_key: command contains control byte at index %d", i)
		}
	}
	return nil
}

// bindKey drives tmuxctl.Controller.BindKey. The handler validates the
// required `key` / `command` lengths and shapes up front so a 100 KB
// payload or a stray NUL fails fast with CodeInvalidParams before any
// tmux command runs; the optional `key_table` is run through the same
// regex/length policy that list_keys uses (validateKeyTable) so the
// two tools share one source of truth for "what does a keytable name
// look like". Failures from tmux itself (unknown command verb, syntax
// error inside the command string) surface verbatim via internalError —
// bind-key has no equivalent of "session not found" (it does not look
// up live state), so we deliberately don't fold any sentinel here.
func (t *Tools) bindKey(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Key        string `json:"key"`
		Command    string `json:"command"`
		KeyTable   string `json:"key_table"`
		Repeatable bool   `json:"repeatable"`
	}
	// Allow an explicit `null` / empty body so a tools/call frame with
	// `arguments: {}` still surfaces the required-field validation
	// below rather than choking on the unmarshal.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("bind_key: %v", err)
		}
	}
	if rerr := validateBindKeyChord(args.Key); rerr != nil {
		return nil, rerr
	}
	if rerr := validateBindKeyCommand(args.Command); rerr != nil {
		return nil, rerr
	}
	// validateKeyTable is shared with list_keys (both want the same
	// "alnum/underscore/dash, ≤64 bytes" shape). It already permits
	// an empty value, which is exactly the "land in the default
	// table" branch BindKey wants — no extra branch needed here.
	if rerr := validateKeyTable(args.KeyTable); rerr != nil {
		return nil, rerr
	}
	if err := t.Ctl.BindKey(ctx, args.Key, args.Command, args.KeyTable, args.Repeatable); err != nil {
		return nil, internalError(fmt.Errorf("bind_key: %w", err))
	}
	return jsonBlock(map[string]any{
		"bound": true,
		"key":   args.Key,
		"table": args.KeyTable,
	})
}

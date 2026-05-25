package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// maxCommandFilterLen caps the up-front length check applied to the
// optional `command` argument so a hostile or buggy caller cannot blow
// up tmux's argv with a megabyte payload before we even reach the
// boundary. tmux's own command verbs are short ("kill-server",
// "list-keys", "respawn-window"); 64 chars is generous while staying
// well under any reasonable shell-arg cap.
const maxCommandFilterLen = 64

// commandFilterRE accepts the same conservative shape tmux's own
// command names use: an alphabetic first character followed by
// alnum / dash. Anything else (whitespace, shell metachars, '/', '..')
// is rejected up-front so a stray quote can never reach tmux's argv.
// The filter name is a single token tmux looks up in its command
// table — there is no reason for it to contain anything more exotic.
var commandFilterRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`)

// listCommandsToolDefs holds the JSON Schema for the list_commands
// tool. It is appended onto the main toolDefs slice via the package
// init() in this file so the registration site stays close to the
// handler — the dispatcher in tools.go only needs the single name →
// handler entry.
var listCommandsToolDefs = []map[string]any{
	{
		"name": "list_commands",
		"description": "Enumerate every command this tmux build advertises via " +
			"`tmux list-commands`. Useful for an agent that needs to introspect " +
			"the tmux command surface before sending one through the rest of " +
			"the boundary, or to confirm a command exists on the deployed " +
			"tmux release. Pass `command` to scope the listing to a single " +
			"command's signature; omit it to list every command. Each entry " +
			"carries `{name, alias, args}` — alias is empty when the command " +
			"has no short form, args is the verbatim flag/argument signature " +
			"tmux printed (empty for no-arg commands like `kill-server`). A " +
			"filter that does not match a known command returns " +
			"`{\"commands\": []}` cleanly rather than an error so callers can " +
			"iterate the response without a separate \"is this an error\" branch.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"maxLength":   maxCommandFilterLen,
					"description": "Optional command name (e.g. \"list-keys\", \"send-keys\"); maps to the trailing positional `tmux list-commands NAME`. Omit to list every command.",
				},
			},
			// Locking additionalProperties keeps the schema strict so
			// an agent that misnames a field (e.g. "name" instead of
			// "command") gets a fast schema-shaped rejection rather
			// than a silent no-op.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register list_commands onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go and the
	// readonly.go allowlist entry) and avoids touching the shared
	// toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, listCommandsToolDefs...)
}

// validateCommandFilter enforces the conservative command-name policy
// for the optional `command` argument. Empty is allowed (the
// controller falls back to the unscoped listing); a non-empty value
// must satisfy the regex/length policy so a stray quote or
// path-injection attempt can't slip through to tmux's argv.
func validateCommandFilter(name string) *rpcError {
	if name == "" {
		return nil
	}
	if len(name) > maxCommandFilterLen {
		return invalidParams("list_commands: command length %d out of range [1..%d]", len(name), maxCommandFilterLen)
	}
	if !commandFilterRE.MatchString(name) {
		return invalidParams("list_commands: command %q must match %s", name, commandFilterRE.String())
	}
	return nil
}

// listCommands drives tmuxctl.Controller.ListCommands and serialises
// the result to the standard
// `{"content":[{"type":"text","text":"<json>"}]}` envelope MCP expects
// from a tools/call. The response shape is a flat object keyed by
// "commands" so a future filter (e.g. a "name_match" regex knob) can
// be added without breaking callers that iterate the list.
//
// Empty stdout (no commands match the requested filter) returns
// `{"commands": []}` cleanly rather than an error so the JSON-RPC
// layer doesn't have to substring-match tmux stderr to tell the cases
// apart. A truly bad invocation (malformed `command` filter) is
// caught up-front by the validator above; anything that slips past
// surfaces via internalError.
func (t *Tools) listCommands(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Command string `json:"command"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}` here, and the zero value means "every command,
	// no filter" which is exactly the `tmux list-commands` no-arg
	// behaviour.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("list_commands: %v", err)
		}
	}
	if rerr := validateCommandFilter(args.Command); rerr != nil {
		return nil, rerr
	}
	cmds, err := t.Ctl.ListCommands(ctx, args.Command)
	if err != nil {
		return nil, internalError(fmt.Errorf("list_commands: %w", err))
	}
	out := make([]map[string]any, 0, len(cmds))
	for _, ci := range cmds {
		out = append(out, map[string]any{
			"name":  ci.Name,
			"alias": ci.Alias,
			"args":  ci.Args,
		})
	}
	return jsonBlock(map[string]any{"commands": out})
}

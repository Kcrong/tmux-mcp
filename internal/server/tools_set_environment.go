package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// maxEnvNameLen caps the length of an environment-variable name. POSIX
// itself doesn't pin a hard limit, but real-world env names top out
// well under 128 bytes; pinning the bound here means a buggy or hostile
// caller cannot smuggle a megabyte-sized identifier through the
// boundary that tmux would happily forward to its argv parser.
const maxEnvNameLen = 128

// envNameRE is shared with tools_display_popup.go (defined there). Both
// tools enforce the same POSIX environment-variable name shape:
// must start with a letter or underscore, then any mix of letters,
// digits, and underscores. tmux itself accepts a wider set of strings,
// but allowing dashes / dots / digits-leading would let a caller
// construct an identifier the surrounding shell can't even reference
// (`$1FOO`, `FOO-BAR`), which silently defeats the purpose of the tool.
// The regex matches the de-facto POSIX rule used by `export`, `set`,
// and most env-injecting CLIs.

// setEnvironmentToolDefs holds the JSON Schema for the set_environment
// tool. The block is appended onto the main toolDefs slice from this
// file's init() so the registration site stays close to the handler —
// the dispatcher in tools.go only needs the single name → handler
// entry.
var setEnvironmentToolDefs = []map[string]any{
	{
		"name": "set_environment",
		"description": "Set or remove an environment variable that future panes will inherit, " +
			"via `tmux set-environment`. scope=session (the default) updates the named " +
			"session's environment table (`-t SESSION`); scope=global updates the " +
			"server-wide table (`-g`) so subsequently created sessions inherit it. " +
			"Pass `value` to set the variable; omit `value` to remove it (`-u NAME`). " +
			"Existing panes keep whatever environment they already have — only newly " +
			"spawned panes pick up the change, matching the underlying tmux semantics.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"maxLength":   maxEnvNameLen,
					"description": "Environment variable name; len 1-128, regex `^[A-Za-z_][A-Za-z0-9_]*$`.",
				},
				"value": map[string]any{
					"type":        "string",
					"description": "Variable value. Omit to remove the variable (`-u NAME`); empty string is a legal value.",
				},
				"scope": map[string]any{
					"type":        "string",
					"enum":        []string{"session", "global"},
					"default":     "session",
					"description": "session = `-t SESSION`; global = `-g`. Defaults to `session`.",
				},
				"session": map[string]any{
					"type":        "string",
					"maxLength":   maxSessionNameLen,
					"description": "Target session for scope=session; ignored for scope=global. Len 1-64, regex `^[A-Za-z0-9_-]+$`.",
				},
			},
			"required": []string{"name"},
			// Lock the schema so a typo'd field (e.g. "val", "var") fails
			// fast with -32602 instead of silently behaving like an
			// unrelated default.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register set_environment onto the main toolDefs slice. Doing this
	// from init() keeps the new tool's surface entirely contained in
	// this file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, setEnvironmentToolDefs...)
}

// validateEnvName enforces the conservative env-name policy: must
// match the POSIX-shaped regex and stay within the length bound.
func validateEnvName(name string) *rpcError {
	if name == "" {
		return invalidParams("name required")
	}
	if len(name) > maxEnvNameLen {
		return invalidParams("name length %d out of range [1..%d]", len(name), maxEnvNameLen)
	}
	if !envNameRE.MatchString(name) {
		return invalidParams("name %q must match %s", name, envNameRE.String())
	}
	return nil
}

// setEnvironmentKnownFields is the closed set of fields set_environment
// accepts. The handler enforces additionalProperties:false at runtime
// by re-decoding into a map and rejecting any key not in this set —
// the schema declaration alone is advisory; clients that ignore it
// must still be rejected at the boundary so a typo'd field doesn't
// silently behave like a default.
var setEnvironmentKnownFields = map[string]struct{}{
	"name":    {},
	"value":   {},
	"scope":   {},
	"session": {},
}

// setEnvironment drives [tmuxctl.Controller.SetEnvironment]. The
// handler validates required fields per scope before reaching tmux:
// `name` is always required and must match the POSIX env-name regex;
// `session` is required for scope=session (the default) and ignored
// for scope=global. Bad inputs surface as -32602 invalidParams,
// matching the dispatch contract every other boundary tool upholds.
//
// `value` semantics: when present the variable is set (even an empty
// string is a legal value tmux will store verbatim); when absent the
// variable is removed via tmux's `-u NAME` form. We tell the two cases
// apart by re-decoding the raw JSON into a presence map — a missing
// key means "remove" while an explicit empty string means "set to
// empty".
//
// Unknown session names surface via the wrapped errs.ErrSessionNotFound
// sentinel, which the JSON-RPC layer maps to CodeSessionNotFound
// (-32000); a tmux refusal for any other reason maps to CodeInternal
// (-32603) through internalError + errs.CodeOf.
//
// The response echoes the resolved name and a removed flag so an
// audit trail / agent loop has the post-state without a follow-up
// `show-environment` round-trip:
// `{"set": true, "name": "FOO", "removed": false}`.
func (t *Tools) setEnvironment(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	// Reject unknown fields up front. The schema already declares
	// additionalProperties:false, but a client that ignores the
	// schema would otherwise see its typo'd field silently default
	// to scope=session / value=remove. Catching it at the boundary
	// gives the typo a -32602 with a clear message.
	if rerr := rejectUnknownEnvFields(raw); rerr != nil {
		return nil, rerr
	}
	var args struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Scope   string `json:"scope"`
		Session string `json:"session"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("set_environment: %v", err)
		}
	}
	if rerr := validateEnvName(args.Name); rerr != nil {
		return nil, rerr
	}
	scope := args.Scope
	if scope == "" {
		// scope is optional in the schema; the documented default is
		// "session" so callers that just want a per-session entry don't
		// have to repeat the field.
		scope = tmuxctl.EnvironmentScopeSession
	}
	switch scope {
	case tmuxctl.EnvironmentScopeSession:
		if rerr := validateSessionRef(args.Session); rerr != nil {
			return nil, rerr
		}
	case tmuxctl.EnvironmentScopeGlobal:
		// Global scope ignores `session` entirely — no extra validation
		// needed.
	default:
		return nil, invalidParams("set_environment: scope %q must be one of session|global", scope)
	}
	// Distinguish "value omitted" (remove form) from "value: ''" (set
	// to empty string). Re-decoding into a presence map keeps the
	// typed args struct above readable while still surfacing the
	// optional-field semantic the spec calls out.
	remove := !setEnvironmentValuePresent(raw)
	// scope=session targets a specific session; rewrite through the
	// configured -session-prefix the same way every other
	// session-targeting tool does, so a multi-tenant deployment routes
	// the set-environment query at the prefixed tmux session the
	// caller actually owns. scope=global ignores session entirely, and
	// an empty session under scope=session has already been rejected
	// by validateSessionRef above.
	resolvedSession := t.resolveSessionRef(args.Session)
	if err := t.Ctl.SetEnvironment(ctx, scope, resolvedSession, args.Name, args.Value, remove); err != nil {
		return nil, internalError(fmt.Errorf("set_environment: %w", err))
	}
	return jsonBlock(map[string]any{
		"set":     true,
		"name":    args.Name,
		"removed": remove,
	})
}

// rejectUnknownEnvFields enforces additionalProperties:false at
// runtime by walking the raw JSON object and complaining about any
// key not in [setEnvironmentKnownFields]. An empty / null body is
// allowed here — the absence of the required `name` field is caught
// downstream by [validateEnvName].
func rejectUnknownEnvFields(raw json.RawMessage) *rpcError {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		// A malformed payload already fails the typed unmarshal in
		// the caller; returning nil here just keeps the error chain
		// consistent (caller produces "set_environment: ..." with the
		// real parser error).
		return nil
	}
	for k := range fields {
		if _, ok := setEnvironmentKnownFields[k]; !ok {
			return invalidParams("set_environment: unknown field %q", k)
		}
	}
	return nil
}

// setEnvironmentValuePresent reports whether the JSON-encoded
// arguments carry an explicit `value` key. We need this to
// distinguish "field omitted" (treat as remove via tmux's -u form)
// from `value: ""` (legal — tmux stores empty strings). Re-decoding
// into a `map[string]json.RawMessage` is the idiomatic way to inspect
// presence without giving up the typed args struct.
func setEnvironmentValuePresent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m["value"]
	return ok
}

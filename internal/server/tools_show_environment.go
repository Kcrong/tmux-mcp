package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// showEnvironmentToolDefs holds the JSON Schema for the
// show_environment tool. The block is appended onto the main toolDefs
// slice from this file's init() so the registration site stays close
// to the handler — the dispatcher in tools.go only needs the single
// name → handler entry. show_environment is the read counterpart of
// set_environment and is on the read-only allowlist (see
// readonly.go); it never mutates server state.
var showEnvironmentToolDefs = []map[string]any{
	{
		"name": "show_environment",
		"description": "Inspect the environment future panes will inherit, via " +
			"`tmux show-environment`. scope=session (the default) reads the " +
			"named target session's environment table (`-t TARGET`); " +
			"scope=global reads the server-wide table (`-g`). Pass `name` to " +
			"narrow to a single variable (`tmux show-environment NAME`); omit " +
			"it to dump the whole table. Removed entries (those tmux marks " +
			"with a leading `-NAME`, typically because the session has " +
			"explicitly hidden an inherited global) come back as `present=false` " +
			"so callers can distinguish them from variables that have never " +
			"been set. Read-only counterpart of `set_environment`.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"maxLength":   maxEnvNameLen,
					"description": "Optional variable name to narrow the response to a single entry; len 1-128, regex `^[A-Za-z_][A-Za-z0-9_]*$`.",
				},
				"scope": map[string]any{
					"type":        "string",
					"enum":        []string{"session", "global"},
					"default":     "session",
					"description": "session = `-t TARGET`; global = `-g`. Defaults to `session`.",
				},
				"target": map[string]any{
					"type":        "string",
					"maxLength":   maxSessionNameLen,
					"description": "Target session for scope=session; ignored for scope=global. Len 1-64, regex `^[A-Za-z0-9_-]+$`.",
				},
			},
			// Lock the schema so a typo'd field (e.g. "session" instead
			// of "target", "var" instead of "name") fails fast with
			// -32602 instead of silently behaving like the default.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register show_environment onto the main toolDefs slice. Doing
	// this from init() keeps the new tool's surface entirely
	// contained in this file (apart from the single dispatcher case
	// in tools.go) and avoids touching the shared toolDefs literal
	// that other PRs are editing.
	toolDefs = append(toolDefs, showEnvironmentToolDefs...)
}

// showEnvironmentKnownFields is the closed set of fields
// show_environment accepts. The handler enforces
// additionalProperties:false at runtime by re-decoding into a map and
// rejecting any key not in this set — the schema declaration alone is
// advisory; clients that ignore it must still be rejected at the
// boundary so a typo'd field doesn't silently behave like a default.
var showEnvironmentKnownFields = map[string]struct{}{
	"name":   {},
	"scope":  {},
	"target": {},
}

// showEnvironment drives [tmuxctl.Controller.ShowEnvironment]. The
// handler validates required fields per scope before reaching tmux:
// `target` is required for scope=session (the default) and ignored
// for scope=global. `name`, when present, must match the same POSIX
// env-name regex set_environment uses so a malformed identifier
// surfaces as -32602 before any tmux command runs.
//
// Response shape depends on whether `name` was supplied:
//
//   - name omitted → `{"vars": {NAME: VALUE, ...}}` for present
//     entries plus `{"removed": ["NAME", ...]}` for entries tmux
//     reports with a leading `-NAME`. Surfacing the two as separate
//     buckets keeps the common "what env will future panes see?"
//     question a single-key lookup while still letting an audit
//     consumer recover the explicit-removal annotations.
//   - name supplied → `{"name": "FOO", "value": "bar", "present":
//     true}` for a tmux-known entry, or `{"name": "FOO", "value":
//     "", "present": false}` if tmux either has no record (the
//     never-set case, surfaced via [tmuxctl.ErrEnvNameNotSet]) or
//     reports the entry as explicitly removed (`-NAME`).
//     Conflating the two on the response keeps the contract
//     "present=false means 'future panes will not see it', period"
//     simple for agent code; callers wanting to distinguish removal
//     from never-set can fall back to the whole-table form.
//
// Unknown session names surface via the wrapped
// errs.ErrSessionNotFound sentinel, which the JSON-RPC layer maps to
// CodeSessionNotFound (-32000). Any other tmux refusal maps to
// CodeInternal (-32603) through internalError + errs.CodeOf.
func (t *Tools) showEnvironment(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	// Reject unknown fields up front. The schema already declares
	// additionalProperties:false, but a client that ignores the
	// schema would otherwise see its typo'd field silently default
	// to scope=session. Catching it at the boundary gives the typo a
	// -32602 with a clear message.
	if rerr := rejectUnknownShowEnvFields(raw); rerr != nil {
		return nil, rerr
	}
	var args struct {
		Name   string `json:"name"`
		Scope  string `json:"scope"`
		Target string `json:"target"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("show_environment: %v", err)
		}
	}
	// `name` is optional. When supplied, run the same regex/length
	// check set_environment uses so the malformed-identifier surface
	// is consistent across the env tool family.
	if args.Name != "" {
		if rerr := validateEnvName(args.Name); rerr != nil {
			return nil, rerr
		}
	}
	scope := args.Scope
	if scope == "" {
		// scope is optional in the schema; the documented default is
		// "session" so callers that just want a per-session lookup
		// don't have to repeat the field.
		scope = tmuxctl.EnvironmentScopeSession
	}
	switch scope {
	case tmuxctl.EnvironmentScopeSession:
		// scope=session requires a target session. Reuse
		// validateSessionRef so the regex/length contract matches
		// every other session-bearing tool (and the message includes
		// the same field-name suffix).
		if rerr := validateSessionRef(args.Target); rerr != nil {
			return nil, rerr
		}
	case tmuxctl.EnvironmentScopeGlobal:
		// Global scope ignores `target` entirely — no extra
		// validation needed.
	default:
		return nil, invalidParams("show_environment: scope %q must be one of session|global", scope)
	}
	// scope=session targets a specific session; rewrite through the
	// configured -session-prefix the same way every other
	// session-targeting tool does, so a multi-tenant deployment
	// routes the show-environment query at the prefixed tmux session
	// the caller actually owns. scope=global ignores target entirely,
	// and an empty target under scope=session has already been
	// rejected by validateSessionRef above, so the resolver is a
	// no-op there.
	resolvedTarget := t.resolveSessionRef(args.Target)
	dump, err := t.Ctl.ShowEnvironment(ctx, args.Name, scope, resolvedTarget)
	if err != nil {
		// "Variable not set in this scope" is a documented
		// successful outcome of the single-name probe form: the
		// caller wanted to know whether FOO is currently set, and
		// the answer is no. Render that as the standard
		// `{name, value, present:false}` shape rather than a wire
		// error so an agent loop doesn't have to special-case it.
		if args.Name != "" && errors.Is(err, tmuxctl.ErrEnvNameNotSet) {
			return jsonBlock(map[string]any{
				"name":    args.Name,
				"value":   "",
				"present": false,
			})
		}
		return nil, internalError(fmt.Errorf("show_environment: %w", err))
	}
	if args.Name != "" {
		// Single-name probe form. tmux exits zero with either
		// `NAME=VALUE` (present) or `-NAME` (removed). Either way
		// the dump has exactly one entry keyed by name; if for some
		// reason tmux returned an empty body, fall back to the
		// not-set shape rather than crashing on a missing key.
		entry, ok := dump.Vars[args.Name]
		if !ok {
			return jsonBlock(map[string]any{
				"name":    args.Name,
				"value":   "",
				"present": false,
			})
		}
		return jsonBlock(map[string]any{
			"name":    args.Name,
			"value":   entry.Value,
			"present": entry.Present,
		})
	}
	// Whole-table form. Split present entries into a `vars` map and
	// removed entries into a separate `removed` slice so the common
	// "what env will future panes see?" question is a single-key
	// lookup. removed is sorted at marshal time by the JSON encoder
	// (it's a slice, so order is preserved verbatim from the dump);
	// callers that need a stable order can sort on their side.
	vars := make(map[string]string, len(dump.Vars))
	removed := make([]string, 0)
	for name, entry := range dump.Vars {
		if entry.Present {
			vars[name] = entry.Value
		} else {
			removed = append(removed, name)
		}
	}
	return jsonBlock(map[string]any{
		"vars":    vars,
		"removed": removed,
	})
}

// rejectUnknownShowEnvFields enforces additionalProperties:false at
// runtime by walking the raw JSON object and complaining about any
// key not in [showEnvironmentKnownFields]. An empty / null body is
// allowed here — the per-scope required-field checks downstream
// produce the right error for the scope=session-without-target case.
func rejectUnknownShowEnvFields(raw json.RawMessage) *rpcError {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		// A malformed payload already fails the typed unmarshal in
		// the caller; returning nil here just keeps the error chain
		// consistent (caller produces "show_environment: ..." with
		// the real parser error).
		return nil
	}
	for k := range fields {
		if _, ok := showEnvironmentKnownFields[k]; !ok {
			return invalidParams("show_environment: unknown field %q", k)
		}
	}
	return nil
}

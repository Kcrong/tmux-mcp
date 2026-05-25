package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// maxSetOptionNameLen caps the option-name length the boundary will
// forward to tmux. tmux's own option names are well under 64 bytes
// (`buffer-limit`, `default-terminal`, `pane-border-status`, etc.) so a
// 128-byte ceiling is generous while still ruling out a malicious
// caller asking us to allocate a multi-MB argv on the parent process.
const maxSetOptionNameLen = 128

// maxSetOptionValueLen caps the value the boundary will forward to
// tmux. tmux options accept anything from booleans (`on` / `off`) to
// long status-format strings; 4 KiB is enough for every realistic case
// (the longest stock format strings are ~1 KiB) and small enough that
// an over-eager caller cannot hide a 100 MB blob inside an argv slot.
const maxSetOptionValueLen = 4096

// setOptionToolDefs holds the JSON Schema for the set_option tool. It
// is appended onto the main toolDefs slice via this file's init() so
// the registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
//
// scope defaults to "session" (the most common case and the one the
// historical CLI defaults to when neither -s nor -w is supplied), but
// the caller may pin "server" / "window" / "pane" explicitly. The
// schema marks `name` required; `value` is required for normal sets
// but suppressed when `unset=true` — this is enforced in the handler
// since JSON Schema cannot express "value required unless unset".
var setOptionToolDefs = []map[string]any{
	{
		"name": "set_option",
		"description": "Set or clear a tmux option via `tmux set-option`. " +
			"scope=server invokes `set-option -s NAME VALUE` (server-wide options); " +
			"scope=session invokes `set-option -t SESSION NAME VALUE` (per-session, default); " +
			"scope=window invokes `set-option -w -t SESSION:WINDOW NAME VALUE`; " +
			"scope=pane invokes `set-option -p -t PANE NAME VALUE` (tmux 3.4+). " +
			"Pass `unset: true` to clear the override (`-u`); `value` is then ignored " +
			"and may be omitted. The `target` field is required for session/window/pane scopes " +
			"and ignored for server scope.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Option name, e.g. `status-interval`. Regex `^[A-Za-z0-9_-]+$`, len 1-128.",
				},
				"value": map[string]any{
					"type":        "string",
					"description": "Option value. Required unless `unset=true`. Capped at 4096 bytes.",
				},
				"scope": map[string]any{
					"type":        "string",
					"enum":        []string{"server", "session", "window", "pane"},
					"default":     "session",
					"description": "Scope to set the option at. Defaults to `session`.",
				},
				"target": map[string]any{
					"type":        "string",
					"description": "Target identifier; required for session/window/pane scopes. session: session name; window: `SESSION:WINDOW`; pane: `SESSION:WINDOW.PANE` or `%N`.",
				},
				"unset": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, clear the override (`-u`) instead of setting a value.",
				},
			},
			"required":             []string{"name"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register set_option onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from a single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, setOptionToolDefs...)
}

// validateSetOptionName enforces the conservative option-name policy.
// Reuses sessionNameRE (alnum + underscore + dash) since tmux option
// names live in the same character set and we want to reject anything
// that could carry a stray quote or shell metachar.
func validateSetOptionName(name string) *rpcError {
	if name == "" {
		return invalidParams("name required")
	}
	if len(name) > maxSetOptionNameLen {
		return invalidParams("name length %d out of range [1..%d]", len(name), maxSetOptionNameLen)
	}
	if !sessionNameRE.MatchString(name) {
		return invalidParams("name %q must match %s", name, sessionNameRE.String())
	}
	return nil
}

// setOption drives [tmuxctl.Controller.SetOption]. The handler does
// the up-front validation per scope so a caller passing a malformed
// target sees CodeInvalidParams (-32602) before any tmux command runs.
//
// Validation order:
//   - `name` is required and must match the conservative regex/length
//     policy.
//   - `value` is required when `unset=false`; when `unset=true` it is
//     ignored (and may be omitted entirely).
//   - `scope` defaults to "session"; explicit values must be in the
//     supported set (server / session / window / pane).
//   - `target` is required for session, window and pane scopes and
//     must satisfy the appropriate regex (session-name shape for
//     scope=session, window-target shape for scope=window, pane-target
//     shape for scope=pane). Server scope ignores target entirely.
//
// Unknown sessions surface via the wrapped errs.ErrSessionNotFound
// sentinel, which the JSON-RPC layer maps to CodeSessionNotFound
// (-32000). Other tmux failures (unknown option name, version
// mismatch, etc.) surface as CodeInternal.
func (t *Tools) setOption(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Scope  string `json:"scope"`
		Target string `json:"target"`
		Unset  bool   `json:"unset"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("set_option: %v", err)
		}
	}
	if rerr := validateSetOptionName(args.Name); rerr != nil {
		return nil, rerr
	}
	if !args.Unset && len(args.Value) > maxSetOptionValueLen {
		return nil, invalidParams("value: max %d bytes", maxSetOptionValueLen)
	}
	// Default scope mirrors the JSON Schema default — handle it here
	// rather than relying on JSON Schema implementations to apply the
	// default (the dispatcher does not run a schema validator before
	// the handler, so the default field stays empty when the caller
	// omits it).
	if args.Scope == "" {
		args.Scope = tmuxctl.OptionScopeSession
	}
	resolvedTarget := args.Target
	switch args.Scope {
	case tmuxctl.OptionScopeServer:
		// Server scope ignores target entirely; do not validate it so
		// a caller that accidentally forwarded a stale `target` field
		// from a session-scope call still succeeds.
	case tmuxctl.OptionScopeSession:
		if rerr := validateSessionRef(args.Target); rerr != nil {
			return nil, rerr
		}
		resolvedTarget = t.resolveSessionRef(args.Target)
	case tmuxctl.OptionScopeWindow:
		if rerr := validateWindowTarget(args.Target); rerr != nil {
			return nil, rerr
		}
		// scope=window targets `SESSION:WINDOW`; rewrite the leading
		// session segment through the configured -session-prefix when
		// the target starts with a session name (i.e. before the colon).
		// Pure window-only targets like `@N` have no session segment to
		// rewrite, so the resolver leaves them alone.
		resolvedTarget = t.resolveWindowMoveTarget(args.Target)
	case tmuxctl.OptionScopePane:
		if rerr := validatePaneTarget(args.Target); rerr != nil {
			return nil, rerr
		}
		// Empty pane target is rejected here (unlike pane_split where
		// it is optional) because set-option -p REQUIRES a concrete
		// target — there is no "current pane" interpretation that
		// would make sense at the boundary.
		if args.Target == "" {
			return nil, invalidParams("target required for scope=pane")
		}
		resolvedTarget = t.resolvePaneTarget(args.Target)
	default:
		return nil, invalidParams("scope %q must be one of server|session|window|pane", args.Scope)
	}

	if err := t.Ctl.SetOption(ctx, args.Name, args.Value, args.Scope, resolvedTarget, args.Unset); err != nil {
		return nil, internalError(fmt.Errorf("set_option: %w", err))
	}
	return jsonBlock(map[string]any{
		"set":   !args.Unset,
		"unset": args.Unset,
		"name":  args.Name,
		"scope": args.Scope,
	})
}

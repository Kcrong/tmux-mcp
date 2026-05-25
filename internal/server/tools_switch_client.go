package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// maxSwitchClientNameLen caps the up-front length check applied to the
// optional `client` argument. tmux client names are TTY paths (e.g.
// "/dev/pts/3", "/dev/ttys001") — typically well under 64 bytes — so a
// 256-byte ceiling leaves comfortable headroom for unusual platforms
// while still bounding the JSON-RPC payload size against a hostile
// caller pasting an unbounded string. Named with a SwitchClient suffix
// so it can coexist with any future client-name policy a neighbouring
// tool (refresh_client, lock_client, …) ships with its own ceiling.
const maxSwitchClientNameLen = 256

// switchClientNameRE accepts the conservative shape a tmux client name
// can take in practice: an absolute path under /dev/, optionally with
// alnum / underscore / dash / dot / colon characters that show up in
// real-world TTY paths (`/dev/pts/3`, `/dev/ttys001`,
// `/dev/tty.usbserial-1410`). We deliberately do NOT accept whitespace,
// shell metachars, glob characters, or backslashes — none of those
// appear in legitimate TTY paths and admitting them would risk stray
// quoting / argv-injection if a future tmux version starts treating
// any of them specially in `-c <client>`.
var switchClientNameRE = regexp.MustCompile(`^/[A-Za-z0-9_./:-]+$`)

// validateSwitchClientName enforces the conservative client-name
// policy for the optional `client` argument on switch_client. Empty is
// allowed (the controller asks tmux to redirect the caller's current
// client, which on a headless server is trivially a no-op); a
// non-empty value must satisfy the regex/length policy so a stray
// quote or path-injection attempt cannot slip through to tmux's argv.
func validateSwitchClientName(name string) *rpcError {
	if name == "" {
		return nil
	}
	if len(name) > maxSwitchClientNameLen {
		return invalidParams("client length %d out of range [1..%d]", len(name), maxSwitchClientNameLen)
	}
	if !switchClientNameRE.MatchString(name) {
		return invalidParams("client %q must match %s", name, switchClientNameRE.String())
	}
	return nil
}

// switchClientToolDefs holds the JSON Schema for the switch_client
// tool. The block is appended onto the main toolDefs slice via the
// init() in this file so the registration site stays close to the
// handler — the dispatcher in tools.go only needs the single name →
// handler entry.
//
// The schema lists every argument as optional at the JSON Schema
// level (none of them are required for a syntactically valid call) but
// the handler enforces "exactly one of {target, last, next, prev}"
// before any tmux invocation. Encoding the rule in the schema would
// require a oneOf / dependencies clause that not every MCP client
// surfaces cleanly; keeping the validation in the handler lets us
// emit a precise -32602 message that names which combination tripped.
var switchClientToolDefs = []map[string]any{
	{
		"name": "switch_client",
		"description": "Redirect a tmux client between sessions on the same server via " +
			"`tmux switch-client [-c <client>] [-t <target>] [-l|-n|-p] [-r]`. " +
			"Use this to bounce an attached terminal from one session to another " +
			"without detaching: pass `target` to land on a specific session, or " +
			"set exactly one of `last` / `next` / `prev` to walk the session list. " +
			"`client` (the path-like name shown in `list_clients`, e.g. \"/dev/pts/0\") " +
			"scopes the redirect to one terminal; omit it to redirect the caller's " +
			"current client. `read_only=true` toggles the client's read-only / " +
			"ignore-size flags (the same `-r` semantics as `attach-session`). " +
			"Returns `{\"switched\": true}` regardless of which path ran. Headless " +
			"servers with nothing attached are a successful no-op — the boundary " +
			"swallows tmux's \"no current client\" stderr so callers can fire-and- " +
			"forget without a separate `list_clients` round-trip.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"client": map[string]any{
					"type":        "string",
					"maxLength":   maxSwitchClientNameLen,
					"description": "Optional tmux client name (typically a TTY path like \"/dev/pts/0\"); regex `^/[A-Za-z0-9_./:-]+$`. Omit to redirect the caller's current client.",
				},
				"target": map[string]any{
					"type":        "string",
					"maxLength":   maxSessionNameLen,
					"description": "Target session name (or a `session:window[.pane]` / `%pane` reference). Required unless exactly one of last/next/prev is true; mutually exclusive with the directional flags.",
				},
				"last": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "Walk to the last (most-recently-visited) session via `-l`. Mutually exclusive with target / next / prev.",
				},
				"next": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "Walk forward to the next session via `-n`. Mutually exclusive with target / last / prev.",
				},
				"prev": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "Walk backward to the previous session via `-p`. Mutually exclusive with target / last / next.",
				},
				"read_only": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "Toggle the client's read-only / ignore-size flags via `-r` on top of the directional choice.",
				},
			},
			// Lock the schema so a typo'd field (e.g. "previous",
			// "readonly") fails fast with -32602 instead of silently
			// behaving like a different directional choice.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register switch_client onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, switchClientToolDefs...)
}

// switchClient drives tmuxctl.Controller.SwitchClient and serialises
// the result to the standard
// `{"content":[{"type":"text","text":"<json>"}]}` envelope MCP expects
// from a tools/call. The response shape is a flat object keyed by
// "switched" so a future addition (e.g. an "echoed_client" field or a
// resolved-target name) can land alongside without breaking callers
// that only read the boolean.
//
// Argument handling:
//   - `client` is optional; when present it must satisfy the
//     conservative regex/length policy (TTY path shape) so a stray
//     quote or path-injection attempt cannot slip through to tmux's
//     argv. When omitted the redirect targets the caller's current
//     client.
//   - `target` is optional at the JSON Schema level but exactly one of
//     {target, last, next, prev} must hold; the handler emits a typed
//     -32602 error naming the violation when the rule is broken. When
//     present and non-empty `target` flows through validateSessionRef
//     so a malformed session string is refused before tmux is asked to
//     resolve it (defence against shell metachars).
//   - `last` / `next` / `prev` are optional booleans; at most one may
//     be true.
//   - `read_only` is an independent toggle (`-r`).
//
// Error mapping:
//   - malformed args / unknown field / zero-or-multiple directional
//     flags → -32602 (invalid params).
//   - named client does not exist → -32000 (CodeSessionNotFound), via
//     the wrapped errs.ErrSessionNotFound the controller emits.
//   - named target session does not exist → -32000 (same code), via
//     the run()-level wrap that already handles isSessionMissingMsg.
//   - any other tmux failure → -32603 (internal).
//
// This is a MUTATING tool (it changes which session a client is bound
// to), so it is deliberately NOT in readOnlyTools — a -read-only
// deployment must reject it before the handler runs.
func (t *Tools) switchClient(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Client   string `json:"client"`
		Target   string `json:"target"`
		Last     bool   `json:"last"`
		Next     bool   `json:"next"`
		Prev     bool   `json:"prev"`
		ReadOnly bool   `json:"read_only"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}` syntactically, but the exactly-one rule below
	// will reject the all-zero case with a precise message.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("switch_client: %v", err)
		}
	}
	if rerr := validateSwitchClientName(args.Client); rerr != nil {
		return nil, rerr
	}
	// Validate target shape only when present — empty means "no -t,
	// rely on a directional flag". When present we reuse the standard
	// session-name policy so a stray colon or shell metachar gets
	// rejected the same way every other session-bearing tool rejects
	// it. validateSessionRef emits "session …" but adapts cleanly to
	// the target field name via its message.
	if args.Target != "" {
		if rerr := validateSessionRef(args.Target); rerr != nil {
			return nil, rerr
		}
	}
	chosen := 0
	if args.Target != "" {
		chosen++
	}
	if args.Last {
		chosen++
	}
	if args.Next {
		chosen++
	}
	if args.Prev {
		chosen++
	}
	if chosen == 0 {
		return nil, invalidParams("switch_client: exactly one of target, last, next, prev must be set")
	}
	if chosen > 1 {
		return nil, invalidParams("switch_client: target, last, next, prev are mutually exclusive (got %d set)", chosen)
	}
	// Resolve the target through the session-prefix machinery so an
	// operator running with -session-prefix can still address logical
	// session names. Empty target stays empty (the directional path
	// owns the redirect in that case). Pane / window-qualified targets
	// (containing ':' or '%') flow through the pane-target rewriter
	// instead — switch-client accepts the same kinds of references and
	// the rewriter already knows how to prefix the session half.
	resolved := args.Target
	if resolved != "" {
		resolved = t.resolvePaneTarget(resolved)
	}
	if err := t.Ctl.SwitchClient(ctx, args.Client, resolved, args.Last, args.Next, args.Prev, args.ReadOnly); err != nil {
		// internalError runs errs.CodeOf so a wrapped
		// ErrSessionNotFound automatically becomes
		// CodeSessionNotFound (-32000); plain controller errors
		// surface as -32603. The named-client and named-target
		// missing branches both flow through that mapping.
		return nil, internalError(fmt.Errorf("switch_client: %w", err))
	}
	return jsonBlock(map[string]any{"switched": true})
}

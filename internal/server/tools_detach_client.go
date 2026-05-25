package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// detachMaxClientNameLen caps the up-front length check applied to the
// optional `client` argument on detach_client. tmux client names are
// TTY paths (e.g. "/dev/pts/3", "/dev/ttys001") — typically well under
// 64 bytes — so a 256-byte ceiling leaves comfortable headroom for
// unusual platforms while still bounding the JSON-RPC payload size
// against a hostile caller pasting an unbounded string. Mirrors the
// cap refresh_client / lock_client use; sized identically so a future
// rebase that consolidates the validators sees no drift.
const detachMaxClientNameLen = 256

// detachClientNameRE accepts the conservative shape a tmux client name
// can take in practice: an absolute path under /dev/, optionally with
// alnum / underscore / dash / dot / colon characters that show up in
// real-world TTY paths (`/dev/pts/3`, `/dev/ttys001`,
// `/dev/tty.usbserial-1410`). We deliberately do NOT accept whitespace,
// shell metachars, glob characters, or backslashes — none of those
// appear in legitimate TTY paths and admitting them would risk stray
// quoting / argv-injection if a future tmux version starts treating
// any of them specially in `-t <client>`.
//
// Sibling regex of refresh_client / lock_client; intentionally
// duplicated here so this file builds standalone against origin/main
// today and so a rebase that lands the other client tools first can
// dedupe in one place.
var detachClientNameRE = regexp.MustCompile(`^/[A-Za-z0-9_./:-]+$`)

// validateDetachClientName enforces the conservative client-name policy
// for the optional `client` argument on detach_client. Empty is
// allowed at the field level (the at-least-one-required rule is
// checked separately); a non-empty value must satisfy the
// regex/length policy so a stray quote or path-injection attempt
// can't slip through to tmux's argv.
func validateDetachClientName(name string) *rpcError {
	if name == "" {
		return nil
	}
	if len(name) > detachMaxClientNameLen {
		return invalidParams("client length %d out of range [1..%d]", len(name), detachMaxClientNameLen)
	}
	if !detachClientNameRE.MatchString(name) {
		return invalidParams("client %q must match %s", name, detachClientNameRE.String())
	}
	return nil
}

// detachClientToolDefs holds the JSON Schema for the detach_client
// tool. It is appended onto the main toolDefs slice via the package
// init() in this file so the registration site stays close to the
// handler — the dispatcher in tools.go only needs the single
// name → handler entry.
//
// All three fields are individually optional in the schema, but the
// handler enforces "at least one of client / session / all must be
// set" before dispatching to tmux. Encoding the at-least-one rule
// purely in JSON Schema would require oneOf/anyOf gymnastics that
// real-world MCP clients render confusingly; doing it at the handler
// boundary keeps the schema flat while still surfacing a clean
// CodeInvalidParams up front.
var detachClientToolDefs = []map[string]any{
	{
		"name": "detach_client",
		"description": "Cleanly end one or more tmux client connections via " +
			"`tmux detach-client [-a] [-s SESSION] [-t CLIENT]` so the " +
			"backing terminal is released. Distinct from `kill_server` " +
			"(tears down the whole daemon) and `lock_client` (holds the " +
			"client but keeps the connection): detach_client severs the " +
			"client/server bond on a per-target basis. Pass `client` " +
			"(the path-like name shown in `list_clients`, e.g. " +
			"\"/dev/pts/0\") to detach exactly one client; pass `session` " +
			"to detach every client attached to that named session; pass " +
			"`all=true` to detach every OTHER client (only meaningful " +
			"with `client`, where it inverts the selection to \"everyone " +
			"except CLIENT\"). At least one of the three must be set — " +
			"a bare {} is rejected up front rather than dispatched as " +
			"`tmux detach-client` (which would target the caller's " +
			"\"current\" client, a concept that does not apply to the " +
			"headless tmux servers tmux-mcp owns). Returns `{\"detached\": true}` " +
			"on success. Headless servers with nothing attached are a " +
			"successful no-op — the boundary swallows tmux's \"no current " +
			"client\" stderr so callers can fire-and-forget without a " +
			"separate `list_clients` round-trip.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"client": map[string]any{
					"type":        "string",
					"maxLength":   detachMaxClientNameLen,
					"description": "Optional tmux client name (typically a TTY path like \"/dev/pts/0\"); regex `^/[A-Za-z0-9_./:-]+$`. Maps to `-t CLIENT`.",
				},
				"session": map[string]any{
					"type":        "string",
					"description": "Optional tmux session name; detaches every client attached to that session via `-s SESSION`. Same conservative regex as the rest of the surface.",
				},
				"all": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, pass `-a` to detach every OTHER client; meaningful only with `client` (inverts the selection to \"everyone except CLIENT\").",
				},
			},
			// Lock the schema so a typo'd field (e.g. "tty",
			// "session_name") fails fast with -32602 instead of silently
			// being ignored.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register detach_client onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, detachClientToolDefs...)
}

// detachClient drives tmuxctl.Controller.DetachClient and serialises
// the result to the standard
// `{"content":[{"type":"text","text":"<json>"}]}` envelope MCP expects
// from a tools/call. The response shape is a flat object keyed by
// "detached" so a future addition (e.g. a count of detached terminals)
// can land alongside without breaking callers that only read the
// boolean.
//
// Argument handling:
//   - `client` is optional; when present it must satisfy the
//     conservative regex/length policy (TTY path shape) so a stray
//     quote or path-injection attempt cannot slip through to tmux's
//     argv.
//   - `session` is optional; when present it must satisfy the
//     existing session-name policy used everywhere else on the
//     surface (alnum/underscore/dash, len 1-64) so the same
//     CodeInvalidParams rejection covers both validators.
//   - `all` is optional, defaults to false; when true the handler
//     passes `-a` to tmux.
//   - At least one of the three must be set — bare `{}` is rejected
//     up front.
//
// Error mapping:
//   - malformed args / unknown field / all-empty → -32602 (invalid params).
//   - named CLIENT does not exist → -32000 (CodeSessionNotFound), via
//     the wrapped errs.ErrSessionNotFound the controller emits.
//   - named SESSION does not exist → SUCCESS (`{"detached": true}`),
//     because tmux folds "no such session" into "no current client" for
//     detach-client and we can't distinguish that case from the
//     legitimate-empty case at the boundary. Callers needing strict
//     missing-session semantics should pre-flight `has_session`.
//   - any other tmux failure → -32603 (internal).
//
// This is a MUTATING tool (it changes the server's client roster), so
// it is deliberately NOT in readOnlyTools — a -read-only deployment
// must reject it before the handler runs.
func (t *Tools) detachClient(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Client  string `json:"client"`
		Session string `json:"session"`
		All     bool   `json:"all"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("detach_client: %v", err)
		}
	}
	// Enforce "at least one input must be set" at the handler layer so
	// the schema stays flat; a JSON Schema oneOf/anyOf encoding would
	// surface in MCP clients as a confusing "exactly one of …" hint
	// even though `client + all` is a legitimate combination.
	if args.Client == "" && args.Session == "" && !args.All {
		return nil, invalidParams("detach_client: at least one of client, session, or all must be set")
	}
	if rerr := validateDetachClientName(args.Client); rerr != nil {
		return nil, rerr
	}
	if args.Session != "" {
		if rerr := validateSessionRef(args.Session); rerr != nil {
			return nil, rerr
		}
	}
	if err := t.Ctl.DetachClient(ctx, args.Client, args.Session, args.All); err != nil {
		return nil, internalError(fmt.Errorf("detach_client: %w", err))
	}
	return jsonBlock(map[string]any{"detached": true})
}

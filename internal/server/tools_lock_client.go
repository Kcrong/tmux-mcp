package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// maxLockClientNameLen caps the up-front length check applied to the
// optional `client` argument. tmux client names are TTY paths (e.g.
// "/dev/pts/3", "/dev/ttys001") — typically well under 64 bytes — so
// a 256-byte ceiling leaves comfortable headroom for unusual platforms
// while still bounding the JSON-RPC payload size against a hostile
// caller pasting an unbounded string. Named with a `LockClient`
// suffix so it can coexist with any future client-name policy a
// neighbouring tool (refresh_client, …) ships with its own ceiling.
const maxLockClientNameLen = 256

// lockClientNameRE accepts the conservative shape a tmux client name
// can take in practice: an absolute path under /dev/, optionally with
// alnum / underscore / dash / dot / colon characters that show up in
// real-world TTY paths (`/dev/pts/3`, `/dev/ttys001`,
// `/dev/tty.usbserial-1410`). We deliberately do NOT accept whitespace,
// shell metachars, glob characters, or backslashes — none of those
// appear in legitimate TTY paths and admitting them would risk stray
// quoting / argv-injection if a future tmux version starts treating
// any of them specially in `-t <client>`.
var lockClientNameRE = regexp.MustCompile(`^/[A-Za-z0-9_./:-]+$`)

// validateLockClientName enforces the conservative client-name policy
// for the optional `client` argument on lock_client. Empty is allowed
// (the controller asks tmux to lock the caller's current client,
// which on a headless server is trivially a no-op); a non-empty value
// must satisfy the regex/length policy so a stray quote or path-
// injection attempt can't slip through to tmux's argv.
func validateLockClientName(name string) *rpcError {
	if name == "" {
		return nil
	}
	if len(name) > maxLockClientNameLen {
		return invalidParams("client length %d out of range [1..%d]", len(name), maxLockClientNameLen)
	}
	if !lockClientNameRE.MatchString(name) {
		return invalidParams("client %q must match %s", name, lockClientNameRE.String())
	}
	return nil
}

// lockClientToolDefs holds the JSON Schema for the lock_client tool.
// It is appended onto the main toolDefs slice via the package init()
// in this file so the registration site stays close to the handler —
// the dispatcher in tools.go only needs the single name → handler
// entry.
var lockClientToolDefs = []map[string]any{
	{
		"name": "lock_client",
		"description": "Lock a single attached tmux client via `tmux lock-client [-t <client>]`. " +
			"Distinct from a session-scoped lock (which would target every client attached " +
			"to a named session): this tool either targets one specific attached client by " +
			"its TTY-path name (the value `list_clients` reports as `tty`, e.g. \"/dev/pts/0\") " +
			"or, with `client` omitted, asks tmux to lock the caller's current client. " +
			"Returns `{\"locked\": true}` regardless of which path ran. Headless servers " +
			"with nothing attached are a successful no-op — the boundary swallows tmux's " +
			"\"no current client\" stderr so callers can fire-and-forget without a separate " +
			"`list_clients` round-trip.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"client": map[string]any{
					"type":        "string",
					"maxLength":   maxLockClientNameLen,
					"description": "Optional tmux client name (typically a TTY path like \"/dev/pts/0\"); regex `^/[A-Za-z0-9_./:-]+$`. Omit to lock the caller's current client.",
				},
			},
			// Lock the schema so a typo'd field (e.g. "client_name") fails
			// fast with -32602 instead of silently behaving like the
			// unscoped variant.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register lock_client onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from the single dispatcher case in tools.go)
	// and avoids touching the shared toolDefs literal that other PRs
	// are editing.
	toolDefs = append(toolDefs, lockClientToolDefs...)
}

// lockClient drives tmuxctl.Controller.LockClient and serialises the
// result to the standard
// `{"content":[{"type":"text","text":"<json>"}]}` envelope MCP
// expects from a tools/call. The response shape is a flat object
// keyed by "locked" so a future addition (e.g. a "client" echo) can
// land alongside without breaking callers that only read the boolean.
//
// Argument handling:
//   - `client` is optional; when present it must satisfy the
//     conservative regex/length policy (TTY path shape) so a stray
//     quote or path-injection attempt cannot slip through to tmux's
//     argv. When omitted the lock targets the caller's current
//     client.
//
// Error mapping:
//   - malformed args / unknown field → -32602 (invalid params).
//   - named client does not exist → -32000 (CodeSessionNotFound), via
//     the wrapped errs.ErrSessionNotFound the controller emits.
//   - any other tmux failure → -32603 (internal).
//
// This is a MUTATING tool (it changes what the client's terminal
// displays — the lock screen replaces the live session view), so it
// is deliberately NOT in readOnlyTools — a -read-only deployment must
// reject it before the handler runs.
func (t *Tools) lockClient(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Client string `json:"client"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}` here, and the zero value of args.Client means
	// "lock the caller's current client".
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("lock_client: %v", err)
		}
	}
	if rerr := validateLockClientName(args.Client); rerr != nil {
		return nil, rerr
	}
	if err := t.Ctl.LockClient(ctx, args.Client); err != nil {
		return nil, internalError(fmt.Errorf("lock_client: %w", err))
	}
	return jsonBlock(map[string]any{"locked": true})
}

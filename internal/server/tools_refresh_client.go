package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// refreshClientMaxNameLen caps the up-front length check applied to the
// optional `client` argument. tmux client names are TTY paths (e.g.
// "/dev/pts/3", "/dev/ttys001") — typically well under 64 bytes — so
// a 256-byte ceiling leaves comfortable headroom for unusual platforms
// while still bounding the JSON-RPC payload size against a hostile
// caller pasting an unbounded string.
//
// The name is prefixed with `refreshClient` to avoid colliding with
// the show_messages helper (`maxClientNameLen`) that lives in the
// same package — both tools accept a `client` argument but the
// regex/length policy is intentionally per-tool so a future tweak to
// one doesn't silently relax the other.
const refreshClientMaxNameLen = 256

// refreshClientNameRE accepts the conservative shape a tmux client name
// can take in practice: an absolute path under /dev/, optionally with
// alnum / underscore / dash / dot / colon characters that show up in
// real-world TTY paths (`/dev/pts/3`, `/dev/ttys001`,
// `/dev/tty.usbserial-1410`). We deliberately do NOT accept whitespace,
// shell metachars, glob characters, or backslashes — none of those
// appear in legitimate TTY paths and admitting them would risk stray
// quoting / argv-injection if a future tmux version starts treating
// any of them specially in `-t <client>`.
//
// The name is prefixed with `refreshClient` to avoid colliding with
// the show_messages helper (`clientNameRE`) defined in the same
// package.
var refreshClientNameRE = regexp.MustCompile(`^/[A-Za-z0-9_./:-]+$`)

// validateRefreshClientName enforces the conservative client-name
// policy for the optional `client` argument on refresh_client. Empty
// is allowed (the controller refreshes every attached client); a
// non-empty value must satisfy the regex/length policy so a stray
// quote or path-injection attempt can't slip through to tmux's argv.
//
// The name is prefixed with `Refresh` to avoid colliding with the
// show_messages helper (`validateClientName`) defined in the same
// package.
func validateRefreshClientName(name string) *rpcError {
	if name == "" {
		return nil
	}
	if len(name) > refreshClientMaxNameLen {
		return invalidParams("client length %d out of range [1..%d]", len(name), refreshClientMaxNameLen)
	}
	if !refreshClientNameRE.MatchString(name) {
		return invalidParams("client %q must match %s", name, refreshClientNameRE.String())
	}
	return nil
}

// refreshClientToolDefs holds the JSON Schema for the refresh_client
// tool. It is appended onto the main toolDefs slice via the package
// init() in this file so the registration site stays close to the
// handler — the dispatcher in tools.go only needs the single
// name → handler entry.
var refreshClientToolDefs = []map[string]any{
	{
		"name": "refresh_client",
		"description": "Force a tmux client redraw via `tmux refresh-client [-S] [-t <client>]`. " +
			"Useful when an agent has rewritten an option that affects what the client " +
			"renders (e.g. `status-format`, `status-style`, `window-status-format`) and " +
			"wants the change to take effect immediately rather than on the next tmux " +
			"render tick. Pass `client` (the path-like name shown in `list_clients`, " +
			"e.g. \"/dev/pts/0\") to scope the refresh to one terminal; omit it to refresh " +
			"every attached client. `status_only=true` redraws just the status line, " +
			"which is faster than a full screen redraw when only status-bar variables " +
			"changed. Returns `{\"refreshed\": true}` regardless of which path ran. " +
			"Headless servers with nothing attached are a successful no-op — the boundary " +
			"swallows tmux's \"no current client\" stderr so callers can fire-and-forget " +
			"without a separate `list_clients` round-trip.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"client": map[string]any{
					"type":        "string",
					"maxLength":   refreshClientMaxNameLen,
					"description": "Optional tmux client name (typically a TTY path like \"/dev/pts/0\"); regex `^/[A-Za-z0-9_./:-]+$`. Omit to refresh every attached client.",
				},
				"status_only": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, pass `-S` to redraw only the status line (faster than a full redraw).",
				},
			},
			// Lock the schema so a typo'd field (e.g. "client_name",
			// "status-only") fails fast with -32602 instead of silently
			// behaving like the unscoped variant.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register refresh_client onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, refreshClientToolDefs...)
}

// refreshClient drives tmuxctl.Controller.RefreshClient and serialises
// the result to the standard
// `{"content":[{"type":"text","text":"<json>"}]}` envelope MCP expects
// from a tools/call. The response shape is a flat object keyed by
// "refreshed" so a future addition (e.g. a "client" echo or a count of
// refreshed terminals) can land alongside without breaking callers
// that only read the boolean.
//
// Argument handling:
//   - `client` is optional; when present it must satisfy the
//     conservative regex/length policy (TTY path shape) so a stray
//     quote or path-injection attempt cannot slip through to tmux's
//     argv. When omitted the refresh runs against every attached
//     client.
//   - `status_only` is optional, defaults to false; when true the
//     handler passes `-S` to tmux for the cheaper status-line refresh.
//
// Error mapping:
//   - malformed args / unknown field → -32602 (invalid params).
//   - named client does not exist → -32000 (CodeSessionNotFound), via
//     the wrapped errs.ErrSessionNotFound the controller emits.
//   - any other tmux failure → -32603 (internal).
//
// This is a MUTATING tool (it changes what the client's terminal
// displays), so it is deliberately NOT in readOnlyTools — a -read-only
// deployment must reject it before the handler runs.
func (t *Tools) refreshClient(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Client     string `json:"client"`
		StatusOnly bool   `json:"status_only"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}` here, and the zero values match "refresh every
	// client, full redraw".
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("refresh_client: %v", err)
		}
	}
	if rerr := validateRefreshClientName(args.Client); rerr != nil {
		return nil, rerr
	}
	if err := t.Ctl.RefreshClient(ctx, args.Client, args.StatusOnly); err != nil {
		return nil, internalError(fmt.Errorf("refresh_client: %w", err))
	}
	return jsonBlock(map[string]any{"refreshed": true})
}

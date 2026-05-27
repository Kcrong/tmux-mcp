package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// serverAccessUserRE mirrors the controller's POSIX `name_regex`. It
// is duplicated here (instead of imported from tmuxctl) so the schema-
// shaped rejection at the dispatcher boundary matches the controller's
// later check exactly. A future divergence between the two would
// surface as a test failure in TestHandle_ServerAccess_RejectsBadUser.
var serverAccessUserRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)

// maxServerAccessUserLen pins the username length cap at the POSIX
// LOGIN_NAME_MAX floor. The same constant lives on the controller
// side; pinning a copy here lets the JSON Schema's `maxLength` get a
// number without reaching across packages.
const maxServerAccessUserLen = 32

// serverAccessOps enumerates the dispatcher's accepted op values. The
// list is the source of truth for both the JSON Schema's enum and
// the handler's switch statement, so a future op (e.g. an alias) only
// needs to be added in one place.
//
// Names mirror the kebab-case style of every other tmux-mcp op-style
// surface but use snake-case for `read_only` because JSON Schema
// enum values are conventionally identifiers, not flags.
var serverAccessOps = []string{"add", "delete", "list", "read_only", "write"}

// serverAccessToolDefs holds the JSON Schema for the server_access
// tool. It is appended onto the main toolDefs slice from this file's
// init() so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
//
// `op` is required and enumerated; `user` is required for every op
// except `list` (where it must be omitted), and the handler enforces
// that mutual constraint up front because plain JSON Schema cannot
// express it. Locking additionalProperties keeps the schema strict so
// an agent that misnames a field (e.g. "username" instead of "user")
// gets a fast schema-shaped rejection rather than a silent no-op.
var serverAccessToolDefs = []map[string]any{
	{
		"name": "server_access",
		"description": "Manage the per-user ACL on the tmux server's shared socket via " +
			"`tmux server-access [-adlrw] [USER]` (tmux 3.4+). Subcommands are dispatched " +
			"via the `op` field: `add` grants USER access (`-a USER`, defaults to read-only " +
			"on tmux's side); `delete` revokes access and detaches any of USER's attached " +
			"clients (`-d USER`); `list` returns the current access table (`-l`, no USER); " +
			"`read_only` switches an existing entry to read-only (`-r USER`); `write` " +
			"switches an existing entry to read+write (`-w USER`). USER must be a POSIX " +
			"username (matches `^[a-z_][a-z0-9_-]*$`, ≤ 32 bytes) and is required for every " +
			"op except `list`. The whole tool mutates server state (only `list` is " +
			"read-only on its own, but the surface is gated as a unit) so it is rejected " +
			"under -read-only.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op": map[string]any{
					"type":        "string",
					"enum":        serverAccessOps,
					"description": "Sub-operation: add, delete, list, read_only, or write.",
				},
				"user": map[string]any{
					"type":        "string",
					"maxLength":   maxServerAccessUserLen,
					"pattern":     serverAccessUserRE.String(),
					"description": "POSIX username (e.g. \"alice\"). Required for every op except `list`; must be omitted when op=list.",
				},
			},
			"required":             []string{"op"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register server_access onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from the single dispatcher case in tools.go)
	// and avoids touching the shared toolDefs literal that other PRs
	// are editing.
	toolDefs = append(toolDefs, serverAccessToolDefs...)
}

// validateServerAccessUser pins the per-call username policy at the
// dispatcher boundary. The controller validator runs the same checks,
// but doing them here too means a malformed USER never reaches tmux's
// argv — and the JSON-RPC layer can hand back a clean -32602 instead
// of internalising the controller's error.
func validateServerAccessUser(user string) *rpcError {
	if user == "" {
		return invalidParams("server_access: user required")
	}
	if len(user) > maxServerAccessUserLen {
		return invalidParams(
			"server_access: user length %d out of range [1..%d]",
			len(user), maxServerAccessUserLen,
		)
	}
	if !serverAccessUserRE.MatchString(user) {
		return invalidParams(
			"server_access: user %q must match %s",
			user, serverAccessUserRE.String(),
		)
	}
	return nil
}

// serverAccess drives every sub-operation of `tmux server-access`. The
// handler dispatches on `op`, validates `user` per branch (required
// everywhere except `list`, where it must be omitted to keep the
// schema and behaviour aligned), and calls into the matching
// Controller method.
//
// Response shapes:
//
//   - add / delete / read_only / write → JSON ack
//     `{"ok": true, "op": "<op>", "user": "<user>"}` so a caller that
//     chains operations can branch on the stable shape rather than
//     parsing a free-form status string.
//   - list → JSON object `{"entries": [{user, permission}, ...]}` —
//     never null, even when the access list is empty, so the caller
//     can range over the slice without a nil-check.
//
// Idempotency. tmux's own `server-access` is not idempotent across the
// board: a duplicate `-a` against an existing entry succeeds quietly
// in some versions and fails with "user already has access" in
// others. The boundary surfaces tmux's stderr verbatim through
// internalError so the caller's recovery loop can inspect the reason
// — wrapping every tmux failure into a "ok" ack here would push the
// idempotency dance into agents which is the wrong layer.
func (t *Tools) serverAccess(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Op   string `json:"op"`
		User string `json:"user"`
	}
	// Allow an absent / empty arguments object so the typed validation
	// below produces the canonical "op required" error rather than a
	// json.Unmarshal complaint.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("server_access: %v", err)
		}
	}
	switch args.Op {
	case "":
		return nil, invalidParams("server_access: op required")
	case "list":
		// `list` is the only branch where USER must be absent — passing
		// one would silently no-op on tmux (it ignores positional args
		// after `-l`) and is treated as a programmer error here for
		// the same reason every other tool refuses contradictory
		// shapes.
		if args.User != "" {
			return nil, invalidParams("server_access: user must be empty when op=list")
		}
		entries, err := t.Ctl.ServerAccessList(ctx)
		if err != nil {
			return nil, internalError(fmt.Errorf("server_access list: %w", err))
		}
		// Force a non-nil slice so the JSON shape is `{"entries":[]}`
		// rather than `{"entries":null}` — easier to range over on the
		// caller side and consistent with every other list_* tool. The
		// tmuxctl.ServerAccessEntry value already carries the right
		// JSON tags, so no conversion is needed beyond the nil-safety.
		if entries == nil {
			entries = []tmuxctl.ServerAccessEntry{}
		}
		return jsonBlock(map[string]any{"entries": entries})
	case "add":
		if rerr := validateServerAccessUser(args.User); rerr != nil {
			return nil, rerr
		}
		if err := t.Ctl.ServerAccessAdd(ctx, args.User); err != nil {
			return nil, internalError(fmt.Errorf("server_access add: %w", err))
		}
	case "delete":
		if rerr := validateServerAccessUser(args.User); rerr != nil {
			return nil, rerr
		}
		if err := t.Ctl.ServerAccessDelete(ctx, args.User); err != nil {
			return nil, internalError(fmt.Errorf("server_access delete: %w", err))
		}
	case "read_only":
		if rerr := validateServerAccessUser(args.User); rerr != nil {
			return nil, rerr
		}
		if err := t.Ctl.ServerAccessReadOnly(ctx, args.User); err != nil {
			return nil, internalError(fmt.Errorf("server_access read_only: %w", err))
		}
	case "write":
		if rerr := validateServerAccessUser(args.User); rerr != nil {
			return nil, rerr
		}
		if err := t.Ctl.ServerAccessWrite(ctx, args.User); err != nil {
			return nil, internalError(fmt.Errorf("server_access write: %w", err))
		}
	default:
		return nil, invalidParams(
			"server_access: op %q must be one of add|delete|list|read_only|write",
			args.Op,
		)
	}
	return jsonBlock(map[string]any{
		"ok":   true,
		"op":   args.Op,
		"user": args.User,
	})
}

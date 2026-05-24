package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// listClientsToolDefs holds the JSON Schema for the list_clients tool.
// It is appended onto the main toolDefs slice from this file's init()
// so the registration site stays close to the handler — the dispatcher
// in tools.go only needs the single name → handler entry.
var listClientsToolDefs = []map[string]any{
	{
		"name": "list_clients",
		"description": "Enumerate clients (attached terminals) visible to this server via " +
			"`tmux list-clients`. Pass `session` to scope the listing to clients " +
			"attached to a single tmux session; omit it to list every client across " +
			"the private tmux server. Each entry includes the controlling TTY, the " +
			"session name the client is bound to, the TERM string the client " +
			"advertised, the current size (cols × rows), the read-only flag, and an " +
			"RFC3339 attachment timestamp. Useful for an agent that needs to confirm " +
			"a human is watching a session before driving it, or to sanity-check " +
			"that the headless server it owns has nothing attached.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"maxLength":   maxSessionNameLen,
					"description": "Optional session name; len 1-64, [A-Za-z0-9_-]. Omit to list every client on the server.",
				},
			},
			// list_clients takes only the optional `session` arg today.
			// Locking additionalProperties keeps the schema strict so an
			// agent that misnames a field gets a fast schema-shaped
			// rejection rather than a silent no-op.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register list_clients onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, listClientsToolDefs...)
}

// listClients drives tmuxctl.Controller.ListClients and serialises the
// result to the standard `{"content":[{"type":"text","text":"<json>"}]}`
// envelope MCP expects from a tools/call. The response shape is a flat
// object keyed by "clients" so a future filter (e.g. "active_only" or a
// "scope" knob) can be added without breaking callers that iterate the
// list.
//
// `session` is optional: when present it must satisfy the same regex /
// length policy as every other session reference; when absent the
// listing covers every client on the server. Empty stdout (no clients
// attached) returns `{"clients": []}` cleanly rather than an error so
// the JSON-RPC layer doesn't have to substring-match tmux stderr to
// tell the cases apart. Unknown session names surface via the wrapped
// errs.ErrSessionNotFound which the dispatcher maps to
// CodeSessionNotFound.
func (t *Tools) listClients(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}` here, and the zero value of args.Session means
	// "list every client on the server".
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("list_clients: %v", err)
		}
	}
	if args.Session != "" {
		if rerr := validateSessionRef(args.Session); rerr != nil {
			return nil, rerr
		}
	}
	clients, err := t.Ctl.ListClients(ctx, args.Session)
	if err != nil {
		return nil, internalError(fmt.Errorf("list_clients: %w", err))
	}
	out := make([]map[string]any, 0, len(clients))
	for _, ci := range clients {
		out = append(out, map[string]any{
			"tty":     ci.TTY,
			"session": ci.Session,
			"term":    ci.Term,
			// Nest cols/rows under "size" so the response shape
			// mirrors the way tmux itself groups the dimensions in
			// its display variables — and so a future addition
			// (e.g. pixel size from `#{client_*pix}`) can land on
			// the same sub-object without disturbing existing keys.
			"size": map[string]any{
				"cols": ci.Width,
				"rows": ci.Height,
			},
			"readonly":      ci.ReadOnly,
			"creation_time": ci.CreatedAt.Format(time.RFC3339),
		})
	}
	return jsonBlock(map[string]any{"clients": out})
}

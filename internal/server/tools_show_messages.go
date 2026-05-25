package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// maxClientNameLen caps the up-front length check applied to the
// optional `client` argument. tmux client identifiers are TTY paths
// (e.g. "/dev/pts/3"); 128 is generous while staying well under any
// reasonable shell-arg cap.
const maxClientNameLen = 128

// clientNameRE bounds the optional `client` argument to a conservative
// shape: alnum, slash, dash, underscore, dot. tmux clients are pinned
// by their TTY path ("/dev/pts/3", "/dev/ttys001") which all fall
// inside this set; restricting it here keeps stray quotes / shell
// metachars / argv-injection attempts from reaching tmux.
var clientNameRE = regexp.MustCompile(`^[A-Za-z0-9/_.\-]+$`)

// validateClientRef enforces the conservative shape on
// show_messages's optional `client` argument. Empty is allowed (every
// client / no client at all on a headless server); a non-empty value
// must satisfy the regex/length policy so a stray quote can never
// reach tmux's argv.
func validateClientRef(client string) *rpcError {
	if client == "" {
		return nil
	}
	if len(client) > maxClientNameLen {
		return invalidParams("show_messages: client length %d out of range [1..%d]", len(client), maxClientNameLen)
	}
	if !clientNameRE.MatchString(client) {
		return invalidParams("show_messages: client %q must match %s", client, clientNameRE.String())
	}
	return nil
}

// showMessagesToolDefs holds the JSON Schema for the show_messages
// tool. It is appended onto the main toolDefs slice via the package
// init() in this file so the registration site stays close to the
// handler — the dispatcher in tools.go only needs the single name →
// handler entry.
var showMessagesToolDefs = []map[string]any{
	{
		"name": "show_messages",
		"description": "Return tmux's per-client message log via `tmux show-messages [-JT] [-t CLIENT]`. " +
			"This is the buffer tmux prints into the bottom status bar; an agent can use it " +
			"to introspect what tmux has been telling clients without having to attach. " +
			"Pass `client` to scope to a specific client (TTY path, e.g. \"/dev/pts/3\"); " +
			"omit it to read the current-client log. Pass `include_jobs=true` to append the " +
			"job log (`-J`); pass `include_terminal=true` to append the terminal log (`-T`). " +
			"On a headless server with nothing attached the response is `{\"messages\": []}` " +
			"rather than an error so the call is safe to issue at any point. " +
			"This tool is read-only.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"client": map[string]any{
					"type":        "string",
					"maxLength":   maxClientNameLen,
					"description": "Optional client id (TTY path); maps to `-t CLIENT`. Omit on a headless server.",
				},
				"include_jobs": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, append the job log (`-J`).",
				},
				"include_terminal": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, append the terminal log (`-T`).",
				},
			},
			// Locking additionalProperties keeps the schema strict so an
			// agent that misnames a field gets a fast schema-shaped
			// rejection rather than a silent no-op.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register show_messages onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from the single dispatcher case in tools.go and
	// the readonly.go allowlist entry) and avoids touching the shared
	// toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, showMessagesToolDefs...)
}

// showMessages drives [tmuxctl.Controller.ShowMessages] and serialises
// the result to the standard `{"content":[{"type":"text","text":"<json>"}]}`
// envelope MCP expects from a tools/call. The response shape is a flat
// object keyed by "messages" (always a list, never null) so the
// JSON-RPC wire stays consistent regardless of whether tmux had any
// messages buffered.
//
// `client`, `include_jobs`, and `include_terminal` are all optional;
// the schema permits `arguments: {}`, in which case the call resolves
// to `tmux show-messages` with no flags — which is the read-only
// inspection of the current-client message log. On a headless server
// that surfaces as a clean empty list rather than an error so an
// agent can issue the call without first having to attach a client.
//
// Unknown client names (only possible when `client` is non-empty)
// surface via the wrapped errs.ErrSessionNotFound which the dispatcher
// maps to CodeSessionNotFound.
func (t *Tools) showMessages(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Client          string `json:"client"`
		IncludeJobs     bool   `json:"include_jobs"`
		IncludeTerminal bool   `json:"include_terminal"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}` here, and the zero values mean "current client,
	// no -J / -T" which matches `tmux show-messages` with no flags.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("show_messages: %v", err)
		}
	}
	if rerr := validateClientRef(args.Client); rerr != nil {
		return nil, rerr
	}
	messages, err := t.Ctl.ShowMessages(ctx, args.Client, args.IncludeJobs, args.IncludeTerminal)
	if err != nil {
		return nil, internalError(fmt.Errorf("show_messages: %w", err))
	}
	// jsonBlock would emit `null` for a nil slice, which would force
	// callers to special-case the response shape. Promote nil to an
	// empty slice so the JSON wire is uniform: `"messages": []` on
	// the headless / nothing-buffered path, never `"messages": null`.
	if messages == nil {
		messages = []string{}
	}
	return jsonBlock(map[string]any{"messages": messages})
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// sourceBufferToolDefs holds the JSON Schema for the source_buffer tool.
// It is appended onto the main toolDefs slice via this file's init() so
// the registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
//
// Paste buffers live on the tmux server (not on a session), so this
// tool deliberately is not session-scoped: there is no `session` field
// in the schema and SessionPrefix does not apply.
//
// `name` is optional (mirroring `tmux source-buffer` itself, which
// picks the most-recently-added buffer when no `-b` is supplied) and
// validated against the same conservative regex/length policy used by
// show_buffer / set_buffer so a stray quote / shell metachar cannot
// slip through to tmux's argv.
var sourceBufferToolDefs = []map[string]any{
	{
		"name": "source_buffer",
		"description": "Read the named tmux paste buffer (or the most-recently-added buffer when " +
			"`name` is omitted) and feed its contents to tmux's command parser as a sequence of " +
			"commands — the same parser that processes lines from `~/.tmux.conf` or " +
			"`tmux source-file`. Wraps `tmux source-buffer [-b NAME]`. Useful for staging dynamic " +
			"config edits in a buffer (via `set_buffer` / `load_buffer`) and applying them " +
			"without writing a file to disk. Buffers live on the tmux server, so this tool is " +
			"not session-scoped. The response is a small `{\"sourced\": true, \"name\": \"<echoed>\"}` " +
			"envelope; `name` is empty when the caller did not pin one. A missing buffer surfaces " +
			"as `-32000` (CodeSessionNotFound); a malformed buffer body (e.g. an unknown tmux " +
			"command) surfaces as `-32603` so clients can branch on the user-input case.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Optional buffer name; len 1-128, regex `^[A-Za-z0-9_-]+$`. Empty / omitted → most-recently-added buffer.",
				},
			},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register source_buffer onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, sourceBufferToolDefs...)
}

// sourceBuffer drives tmuxctl.Controller.SourceBuffer. The optional
// `name` is validated up front (via the existing validateBufferName
// helper, shared with show_buffer) so a malformed value sees
// CodeInvalidParams (-32602) before any tmux command runs; an empty
// `name` resolves to `tmux source-buffer` with no -b, which tmux maps
// to "the most-recently-added buffer".
//
// The response echoes the requested name back verbatim (empty when the
// caller did not pin one) so an agent that round-trips a name through
// list_buffers → source_buffer can correlate the response without an
// extra bookkeeping step. We deliberately do NOT resolve the auto-name
// here the way set_buffer does: source-buffer does not change the
// buffer table (it only feeds the body to the parser), so there is no
// new auto-counter to recover.
func (t *Tools) sourceBuffer(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Name string `json:"name"`
	}
	// Allow an explicit `null` / empty body so a tools/call frame with
	// `arguments: {}` flows through to the controller (which then runs
	// `tmux source-buffer` with no -b — i.e. the documented "pick the
	// most-recent buffer" default) rather than choking on the unmarshal.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("source_buffer: %v", err)
		}
	}
	if rerr := validateBufferName(args.Name); rerr != nil {
		return nil, invalidParams("source_buffer: %s", rerr.Message)
	}
	if err := t.Ctl.SourceBuffer(ctx, args.Name); err != nil {
		return nil, internalError(fmt.Errorf("source_buffer: %w", err))
	}
	return jsonBlock(map[string]any{
		"sourced": true,
		"name":    args.Name,
	})
}

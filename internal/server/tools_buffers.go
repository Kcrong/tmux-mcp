package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// maxBufferNameLen caps the up-front length check applied to a
// caller-supplied buffer name. tmux is fussy about names that contain
// shell metachars or trailing whitespace, so we restrict the boundary
// to a conservative alnum/underscore/dash set; 128 chars is generous
// (tmux's own auto names are "bufferN" with N capped at the buffer
// count) while still bounding the JSON-RPC payload size.
const maxBufferNameLen = 128

// bufferNameRE accepts the same conservative shape used elsewhere on
// the boundary: alnum, underscore, dash. The regex deliberately does
// NOT permit colons, dots, or whitespace — none of those are valid in
// a tmux buffer name and letting them through would risk stray
// quoting / argv-injection if a future tmux version starts treating
// colons in buffer names as a target separator the way it already
// does for sessions/windows.
var bufferNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// buffersToolDefs holds the JSON Schemas for the buffer-paste tools.
// They are appended onto the main toolDefs slice via the package
// init() in this file so the registration site stays close to the
// handlers — and the dispatcher in tools.go only needs the two
// name → handler entries.
var buffersToolDefs = []map[string]any{
	{
		"name": "list_buffers",
		"description": "Enumerate the paste buffers tmux is currently holding on this server. " +
			"Each entry carries the tmux-assigned (or caller-pinned) name, the byte size, " +
			"and the creation timestamp as RFC3339 (UTC). Pair with show_buffer to read a " +
			"specific buffer's contents. Returns `{\"buffers\": []}` (not an error) when " +
			"no buffers have been stored yet.",
		"inputSchema": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	},
	{
		"name": "show_buffer",
		"description": "Return the raw text content of a tmux paste buffer. Omit `name` (or pass " +
			"an empty string) to dump the most-recently-added buffer, matching the tmux CLI " +
			"default — the common case after a fresh `set-buffer`. When `name` is supplied, " +
			"`tmux show-buffer -b <name>` runs and the value round-trips verbatim. Pair " +
			"with list_buffers to discover the available names.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Optional buffer name; defaults to the most-recently-added buffer.",
				},
			},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register the buffer tools onto the main toolDefs slice. Doing
	// this from init() keeps the new tool surface entirely contained
	// in this file (apart from the two dispatcher cases in tools.go)
	// and avoids touching the shared toolDefs literal that other PRs
	// are editing.
	toolDefs = append(toolDefs, buffersToolDefs...)
}

// validateBufferName enforces the conservative buffer-name policy for
// the optional `name` argument on show_buffer. Empty is allowed (the
// controller falls back to dumping the most-recently-added buffer); a
// non-empty value must satisfy the regex/length policy so a stray
// quote or path-injection attempt can't slip through to tmux's argv.
func validateBufferName(name string) *rpcError {
	if name == "" {
		return nil
	}
	if len(name) > maxBufferNameLen {
		return invalidParams("buffer name length %d out of range [1..%d]", len(name), maxBufferNameLen)
	}
	if !bufferNameRE.MatchString(name) {
		return invalidParams("buffer name %q must match %s", name, bufferNameRE.String())
	}
	return nil
}

// listBuffers drives tmuxctl.Controller.ListBuffers and serialises the
// result to the {content: [{type: text, text: "<json>"}]} envelope MCP
// expects from a tools/call. The shape is intentionally a flat object
// keyed by "buffers" so a future addition (e.g. a "scope" filter) does
// not break callers that iterate the list.
//
// json.Unmarshal on an empty payload is fine — both the schema and
// the dispatcher allow `arguments: {}` here, and the tool itself
// takes no arguments. We accept (and ignore) any well-formed JSON
// object so the dispatcher does not have to special-case
// no-argument tools at the framing layer.
func (t *Tools) listBuffers(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	if len(raw) > 0 {
		var probe struct{}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, invalidParams("list_buffers: %v", err)
		}
	}
	bufs, err := t.Ctl.ListBuffers(ctx)
	if err != nil {
		return nil, internalError(fmt.Errorf("list_buffers: %w", err))
	}
	out := make([]map[string]any, 0, len(bufs))
	for _, b := range bufs {
		out = append(out, map[string]any{
			"name":       b.Name,
			"size":       b.Size,
			"created_at": b.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return jsonBlock(map[string]any{"buffers": out})
}

// showBuffer drives tmuxctl.Controller.ShowBuffer. The optional `name`
// is validated up front so a malformed value sees CodeInvalidParams
// (-32602) before any tmux command runs; an empty `name` resolves to
// `tmux show-buffer` with no -b, dumping the most-recently-added
// buffer.
//
// The response carries both the requested name (echoed back when the
// caller pinned one, empty otherwise) and the buffer's `data` payload,
// matching the documented `{ "name": "...", "data": "..." }` shape so
// agents that round-trip a name through list_buffers → show_buffer
// can correlate the response without an extra bookkeeping step.
func (t *Tools) showBuffer(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Name string `json:"name"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("show_buffer: %v", err)
		}
	}
	if rerr := validateBufferName(args.Name); rerr != nil {
		return nil, invalidParams("show_buffer: %s", rerr.Message)
	}
	body, err := t.Ctl.ShowBuffer(ctx, args.Name)
	if err != nil {
		return nil, internalError(fmt.Errorf("show_buffer: %w", err))
	}
	return jsonBlock(map[string]any{
		"name": args.Name,
		"data": body,
	})
}

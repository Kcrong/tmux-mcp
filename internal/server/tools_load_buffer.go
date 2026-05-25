package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// maxLoadBufferDataBytes caps the `data` payload load_buffer accepts.
// 1 MiB matches set_buffer's policy — the MCP layer's reasoning is the
// same (clipboard snippets, not megabyte log dumps), and keeping the
// two ceilings aligned means a caller picking between set_buffer and
// load_buffer doesn't have to think about which boundary is more
// permissive. Note this is the JSON-RPC frame ceiling, NOT the
// underlying argv limit — load_buffer's whole reason for existing is
// to dodge the OS argv length cap, but the MCP layer still wants a
// predictable size for the wire format.
const maxLoadBufferDataBytes = 1 << 20 // 1 MiB

// loadBufferToolDefs holds the JSON Schema for the load_buffer tool.
// It is appended onto the main toolDefs slice via this file's init()
// so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
//
// Paste buffers live on the tmux server (not on a session), so this
// tool deliberately is not session-scoped: there is no `session`
// field in the schema and SessionPrefix does not apply.
var loadBufferToolDefs = []map[string]any{
	{
		"name": "load_buffer",
		"description": "Inject `data` into a tmux paste buffer via `tmux load-buffer -b NAME -` " +
			"(payload streamed over the child's stdin). Behaviourally identical to set_buffer — " +
			"same `name` / `append` semantics, same `bufferN` auto-naming when `name` is omitted — " +
			"but the bytes travel through stdin instead of a positional argv argument so very " +
			"large payloads do not run into the OS argv length cap (ARG_MAX, ~128 KiB on Linux). " +
			"Reach for load_buffer when you have a kilobyte-sized blob (a captured logfile slice, " +
			"a screenful of source code, a binary-safe snippet) and want a discrete tool call to " +
			"land it in a buffer; reach for set_buffer when the payload is small and you prefer " +
			"the single-argv shape. The response echoes the resolved buffer name " +
			"(`{\"loaded\": true, \"name\": \"<resolved>\"}`) so a follow-up `show_buffer` can " +
			"target the exact buffer that was written.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"data": map[string]any{
					"type":        "string",
					"description": "Buffer payload, streamed verbatim over stdin. Empty string is allowed. Capped at 1 MiB.",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Optional buffer name; omit to let tmux assign `bufferN`. Regex `^[A-Za-z0-9_-]+$`, len 1-128.",
				},
				"append": map[string]any{
					"type":        "boolean",
					"description": "When true, append to an existing buffer (`-a`) instead of replacing it.",
					"default":     false,
				},
			},
			"required":             []string{"data"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register load_buffer onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from a single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, loadBufferToolDefs...)
}

// loadBuffer drives tmuxctl.Controller.LoadBuffer. The handler
// validates the required `data` length up front so a 100 MB blob fails
// fast with CodeInvalidParams (-32602) before any tmux command runs;
// a caller-supplied `name` is also validated against the same
// conservative regex/length policy set_buffer uses so a stray quote
// or shell metachar cannot slip through to tmux's argv. The response
// echoes the resolved buffer name so a follow-up `show_buffer` can
// target the exact buffer that was written, which matters for the
// auto-name path where the caller does not know what tmux assigned.
func (t *Tools) loadBuffer(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Data   string `json:"data"`
		Name   string `json:"name"`
		Append bool   `json:"append"`
	}
	// Allow an explicit `null` / empty body so a tools/call frame with
	// `arguments: {}` still surfaces the required-field validation
	// below rather than choking on the unmarshal.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("load_buffer: %v", err)
		}
	}
	// `data` is the only required field; tmux happily accepts an
	// empty payload (it really does store one when load-buffer reads
	// 0 bytes from stdin), so we only reject the "field absent
	// entirely" case. Re-decoding into a presence-check struct keeps
	// us aligned with set_buffer's contract: a caller that genuinely
	// wants "load an empty buffer" can pass `data: ""` explicitly
	// without surprising us.
	if !loadBufferDataPresent(raw) {
		return nil, invalidParams("data: required")
	}
	if len(args.Data) > maxLoadBufferDataBytes {
		return nil, invalidParams("data: max %d bytes", maxLoadBufferDataBytes)
	}
	// Reuse set_buffer's name validator: the policy is identical
	// (alnum / underscore / dash, len 1-128) and duplicating the regex
	// here would let the two surfaces drift.
	if rerr := validateSetBufferName(args.Name); rerr != nil {
		return nil, rerr
	}
	resolved, err := t.Ctl.LoadBuffer(ctx, args.Data, args.Name, args.Append)
	if err != nil {
		return nil, internalError(fmt.Errorf("load_buffer: %w", err))
	}
	return jsonBlock(map[string]any{
		"loaded": true,
		"name":   resolved,
	})
}

// loadBufferDataPresent reports whether the JSON-encoded arguments
// carry an explicit `data` key. We need this to distinguish "field
// omitted" (must reject) from `data: ""` (legal — tmux stores empty
// buffers). Re-decoding into a `map[string]json.RawMessage` is the
// idiomatic way to inspect presence without giving up the typed args
// struct above.
func loadBufferDataPresent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		// A malformed payload already fails the typed unmarshal in
		// the caller; returning false here just keeps the error
		// chain consistent (caller produces "load_buffer: ...").
		return false
	}
	_, ok := m["data"]
	return ok
}

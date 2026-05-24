package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// maxSetBufferDataBytes caps the `data` payload set_buffer accepts.
// 1 MiB matches the documented contract — large enough to stash any
// realistic clipboard snippet (a screenful of source code is ~50 KB,
// a generous shell history ~256 KB) and small enough that a hostile
// or buggy caller cannot pin the JSON-RPC writer copying tens of MB
// of payload bytes into a tmux buffer the controller will then have
// to re-marshal on every list-buffers walk.
const maxSetBufferDataBytes = 1 << 20 // 1 MiB

// maxSetBufferNameLen mirrors maxBufferNameLen on PR #98's read-side
// tools but is duplicated locally so this PR remains self-contained
// while feat/buffer-tools is still in flight. When the broader buffer
// surface lands, the constant deduplication is a one-line follow-up.
const maxSetBufferNameLen = 128

// setBufferNameRE accepts the same conservative shape used elsewhere
// on the boundary: alnum, underscore, dash. The regex deliberately
// does NOT permit colons, dots, or whitespace — none of those are
// valid in a tmux buffer name and letting them through would risk
// stray quoting / argv-injection if a future tmux version starts
// treating colons in buffer names as a target separator the way it
// already does for sessions/windows.
var setBufferNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// setBufferToolDefs holds the JSON Schema for the set_buffer tool. It
// is appended onto the main toolDefs slice via this file's init() so
// the registration site stays close to the handler — the dispatcher
// in tools.go only needs the single name → handler entry.
//
// Paste buffers live on the tmux server (not on a session), so this
// tool deliberately is not session-scoped: there is no `session`
// field in the schema and SessionPrefix does not apply.
var setBufferToolDefs = []map[string]any{
	{
		"name": "set_buffer",
		"description": "Write `data` into a tmux paste buffer via `tmux set-buffer`. Buffers live on " +
			"the tmux server (not on a session) so a caller can later read the value back with " +
			"`show_buffer` from any session — useful for stashing large clipboard-style " +
			"snippets between tool calls without serialising them through repeated " +
			"send_keys frames. Pass an optional `name` to pin a stable buffer name (`-b NAME`); " +
			"omit it to let tmux auto-assign `bufferN`. Set `append=true` to concatenate onto " +
			"an existing buffer (`-a`); when the named buffer does not exist tmux creates it, " +
			"matching the underlying CLI semantics. The response echoes the resolved buffer name " +
			"(`{\"set\": true, \"name\": \"<resolved>\"}`) so a follow-up `show_buffer` can target " +
			"the exact buffer that was written.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"data": map[string]any{
					"type":        "string",
					"description": "Buffer payload; empty string is allowed. Capped at 1 MiB.",
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
	// Register set_buffer onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from a single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, setBufferToolDefs...)
}

// validateSetBufferName enforces the conservative buffer-name policy
// for set_buffer's optional `name` argument. Empty is allowed (the
// controller falls back to tmux's auto-naming); a non-empty value
// must satisfy the regex/length policy so a stray quote or path-
// injection attempt can't slip through to tmux's argv.
func validateSetBufferName(name string) *rpcError {
	if name == "" {
		return nil
	}
	if len(name) > maxSetBufferNameLen {
		return invalidParams("name length %d out of range [1..%d]", len(name), maxSetBufferNameLen)
	}
	if !setBufferNameRE.MatchString(name) {
		return invalidParams("name %q must match %s", name, setBufferNameRE.String())
	}
	return nil
}

// setBuffer drives tmuxctl.Controller.SetBuffer. The handler validates
// the required `data` length up front so a 100 MB blob fails fast with
// CodeInvalidParams (-32602) before any tmux command runs; a
// caller-supplied `name` is also validated against the conservative
// regex/length policy so a stray quote or shell metachar cannot slip
// through to tmux's argv. The response echoes the resolved buffer name
// so a follow-up `show_buffer` can target the exact buffer that was
// written, which matters for the auto-name path where the caller does
// not know what tmux assigned.
func (t *Tools) setBuffer(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
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
			return nil, invalidParams("set_buffer: %v", err)
		}
	}
	// `data` is the only required field; tmux happily accepts an
	// empty buffer (it really does store one), so we only reject the
	// "field absent entirely" case. We can distinguish absent vs.
	// empty-string by re-decoding into a second struct that captures
	// whether the field was present, but the schema's `required:
	// ["data"]` list already covers most clients — and a caller that
	// genuinely wants "set an empty buffer" can pass `data: ""`
	// explicitly without surprising us.
	if !setBufferDataPresent(raw) {
		return nil, invalidParams("data: required")
	}
	if len(args.Data) > maxSetBufferDataBytes {
		return nil, invalidParams("data: max %d bytes", maxSetBufferDataBytes)
	}
	if rerr := validateSetBufferName(args.Name); rerr != nil {
		return nil, rerr
	}
	resolved, err := t.Ctl.SetBuffer(ctx, args.Data, args.Name, args.Append)
	if err != nil {
		return nil, internalError(fmt.Errorf("set_buffer: %w", err))
	}
	return jsonBlock(map[string]any{
		"set":  true,
		"name": resolved,
	})
}

// setBufferDataPresent reports whether the JSON-encoded arguments
// carry an explicit `data` key. We need this to distinguish "field
// omitted" (must reject) from `data: ""` (legal — tmux stores empty
// buffers). Re-decoding into a `map[string]json.RawMessage` is the
// idiomatic way to inspect presence without giving up the typed
// args struct above.
func setBufferDataPresent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		// A malformed payload already fails the typed unmarshal in
		// the caller; returning false here just keeps the error
		// chain consistent (caller produces "set_buffer: ...").
		return false
	}
	_, ok := m["data"]
	return ok
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// pasteBufferToolDefs holds the JSON Schema for the paste_buffer tool.
// It is appended onto the main toolDefs slice from this file's init()
// so the registration site stays close to the handler — the dispatcher
// in tools.go only needs the single name → handler entry, mirroring
// the convention every other tool file in this package follows.
//
// paste_buffer mutates pty state on the targeted pane (it forwards the
// stored bytes through tmux's paste machinery, which delivers them to
// whatever process is reading the pane's tty), so the tool is
// deliberately omitted from the read-only allowlist in readonly.go —
// an agent constrained to inspection cannot smuggle keystrokes into a
// session via a "paste" relabel.
var pasteBufferToolDefs = []map[string]any{
	{
		"name": "paste_buffer",
		"description": "Inject the contents of a tmux paste buffer into the targeted pane via " +
			"`tmux paste-buffer [-d] [-p] [-b NAME] -t TARGET`. Useful when an agent has staged " +
			"a snippet via `set_buffer` and now wants to deliver it into a running shell or TUI " +
			"without paying the per-keystroke cost of `send_keys` on a long payload. " +
			"`target` is a tmux pane-target string (\"session\", \"session:window\", or " +
			"\"session:window.pane\"). `buffer_name` is optional — omit it to paste the " +
			"most-recently-added buffer (the bare `paste-buffer` CLI default), or pin a " +
			"specific name to target the buffer set under `set_buffer -b NAME`. Set " +
			"`delete_after=true` to drop the buffer from tmux's list once the paste lands " +
			"(`-d`), the idiomatic \"use once and discard\" pattern for ephemeral snippets. " +
			"Set `bracketed=true` to wrap the bytes in bracketed-paste escape sequences " +
			"(`-p`) when the receiving application supports it. Returns a small JSON ack " +
			"`{\"pasted\": true}` on success.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Pane target (\"session\", \"session:window\", or \"session:window.pane\").",
				},
				"buffer_name": map[string]any{
					"type":        "string",
					"description": "Optional buffer name; omit to paste the most-recently-added buffer. Regex `^[A-Za-z0-9_-]+$`, len 1-128.",
				},
				"delete_after": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, delete the buffer after pasting (`-d`).",
				},
				"bracketed": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, wrap the paste in bracketed-paste escape sequences (`-p`).",
				},
			},
			"required":             []string{"target"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register paste_buffer onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, pasteBufferToolDefs...)
}

// pasteBuffer drives tmuxctl.Controller.PasteBuffer. The handler does
// the usual up-front validation: target must be non-empty and pass the
// pane-target regex, buffer_name (when supplied) must satisfy the
// conservative buffer-name policy. Both checks run before any tmux
// command so a malformed value sees CodeInvalidParams (-32602) rather
// than a stray "tmux: invalid option" surfacing as -32603. A missing
// pane / session / buffer surfaces as CodeSessionNotFound (-32000) via
// internalError → errs.CodeOf, mirroring the show_buffer / pane_swap
// contract for "the named thing does not exist on this server".
func (t *Tools) pasteBuffer(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target      string `json:"target"`
		BufferName  string `json:"buffer_name"`
		DeleteAfter bool   `json:"delete_after"`
		Bracketed   bool   `json:"bracketed"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("paste_buffer: %v", err)
	}
	if args.Target == "" {
		return nil, invalidParams("paste_buffer: target required")
	}
	if rerr := validatePaneTarget(args.Target); rerr != nil {
		return nil, invalidParams("paste_buffer: %s", rerr.Message)
	}
	// Reuse the buffer-name validator that show_buffer already pins.
	// validateBufferName allows empty (which is the documented default
	// for paste_buffer too: pasting the most-recently-added buffer), so
	// we get the regex/length guard "for free" only when the caller
	// actually pinned a name.
	if rerr := validateBufferName(args.BufferName); rerr != nil {
		return nil, invalidParams("paste_buffer: %s", rerr.Message)
	}
	if err := t.Ctl.PasteBuffer(
		ctx,
		t.resolvePaneTarget(args.Target),
		args.BufferName,
		args.DeleteAfter,
		args.Bracketed,
	); err != nil {
		return nil, internalError(fmt.Errorf("paste_buffer: %w", err))
	}
	return jsonBlock(map[string]any{"pasted": true})
}

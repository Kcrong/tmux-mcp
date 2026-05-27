package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxPipePaneShellCommandLen caps the `shell_command` payload pipe_pane
// accepts. 4 KiB is generous for any realistic logging pipeline (`tee`,
// `cat >`, a piped grep, etc.) and small enough that a hostile or buggy
// caller cannot pin the JSON-RPC writer copying multi-megabyte argv into
// tmux's command socket. Matches the maxDisplayFormatLen ceiling so the
// boundary's "free-form string" budget stays consistent across tools.
const maxPipePaneShellCommandLen = 4096

// pipePaneToolDefs holds the JSON Schema for the pipe_pane tool. It is
// appended onto the main toolDefs slice from this file's init() so the
// registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
//
// Mutating in spirit (it spawns a shell pipeline on the tmux server),
// so it is **not** in the read-only allowlist — operators running
// -read-only must keep it gated away from inspection-only agents.
var pipePaneToolDefs = []map[string]any{
	{
		"name": "pipe_pane",
		"description": "Pipe a pane's output through a shell command via `tmux pipe-pane [-IO] -t <target> " +
			"[shell-command]`. The canonical way to log pane output to a file: " +
			"`{\"target\": \"demo:0\", \"shell_command\": \"cat > /tmp/demo.log\"}` flushes every byte " +
			"tmux writes to the pty into `/tmp/demo.log` until a follow-up call clears the pipe. " +
			"Calling pipe_pane with `shell_command` empty/omitted sends a bare `pipe-pane` to tmux, " +
			"which tears down any existing pipe on that pane (the documented \"stop logging\" form). " +
			"`output_only=true` adds `-O` so only output written by tmux is piped, not input typed at " +
			"the pane; `also_input=true` adds `-I` so input is ALSO piped — combine both to mirror " +
			"the entire pty in both directions. CAUTION: tmux runs `shell_command` via `/bin/sh -c`; " +
			"the command itself is not sandboxed by this server, so operators must trust the agents " +
			"that can call this tool. Use `-allowlist` to gate the surface away from untrusted " +
			"clients. Returns a small JSON ack `{\"piped\": true}` on success.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Pane target (\"session\", \"session:window\", or \"session:window.pane\").",
				},
				"shell_command": map[string]any{
					"type": "string",
					"description": "Shell pipeline tmux runs via /bin/sh -c. Capped at 4096 bytes; NUL bytes " +
						"and other ASCII control characters (except tab) are rejected. Omit or pass an " +
						"empty string to STOP an existing pipe.",
					"maxLength": maxPipePaneShellCommandLen,
				},
				"output_only": map[string]any{
					"type":        "boolean",
					"description": "When true, only output written by tmux is piped (`-O`). Default false (tmux's default semantics apply).",
					"default":     false,
				},
				"also_input": map[string]any{
					"type":        "boolean",
					"description": "When true, also pipe input typed at the pane (`-I`). Combine with `output_only` to mirror both directions.",
					"default":     false,
				},
			},
			"required":             []string{"target"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register pipe_pane onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, pipePaneToolDefs...)
}

// validatePipePaneShellCommand enforces the boundary policy for the
// optional `shell_command` argument. Empty is allowed (the documented
// "stop the existing pipe" form); a non-empty value must be valid UTF-8,
// fit under the 4 KiB ceiling, and contain no NUL bytes or other ASCII
// control characters (HT/0x09 is allowed because real-world shell lines
// occasionally use it, but everything else in 0x00..0x1F and 0x7F is
// rejected up front so a stray null/escape can't break the JSON-RPC
// frame or smuggle past tmux's argv parser).
func validatePipePaneShellCommand(cmd string) *rpcError {
	if cmd == "" {
		return nil
	}
	if len(cmd) > maxPipePaneShellCommandLen {
		return invalidParams("shell_command length %d exceeds %d",
			len(cmd), maxPipePaneShellCommandLen)
	}
	if !utf8.ValidString(cmd) {
		return invalidParams("shell_command must be valid UTF-8")
	}
	for i, r := range cmd {
		if r == 0 {
			return invalidParams("shell_command must not contain NUL bytes (offset %d)", i)
		}
		// Allow horizontal tab (0x09) for spacing in real-world pipelines;
		// reject every other ASCII control char (0x00..0x1F, 0x7F) so a
		// stray newline / ESC can't break the JSON-RPC frame or sneak
		// past tmux's argv parser. Higher Unicode codepoints (e.g. CJK)
		// pass through unchanged — tmux happily forwards them to /bin/sh.
		if r != '\t' && (r < 0x20 || r == 0x7F) {
			return invalidParams("shell_command must not contain control character %#02x (offset %d)", r, i)
		}
	}
	return nil
}

// pipePane drives tmuxctl.Controller.PipePane. The handler validates the
// required `target` shape and the optional `shell_command` payload up
// front so a caller passing a malformed value sees CodeInvalidParams
// (-32602) before any tmux command runs. The response is a small JSON
// ack `{"piped": true}`; the boundary deliberately does not echo the
// resolved argv because tmux gives no useful confirmation back and a
// follow-up read of the operator's log file is the natural way to
// confirm the pipe is flowing.
//
// Mutating: pipe_pane spawns a shell pipeline on the tmux server. The
// COMMAND ITSELF IS NOT SANDBOXED — operators must trust the agents
// calling this tool. The -allowlist flag is the documented way to gate
// it away from untrusted clients.
func (t *Tools) pipePane(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target       string `json:"target"`
		ShellCommand string `json:"shell_command"`
		OutputOnly   bool   `json:"output_only"`
		AlsoInput    bool   `json:"also_input"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("pipe_pane: %v", err)
	}
	if args.Target == "" {
		return nil, invalidParams("pipe_pane: target required")
	}
	if rerr := validatePaneTarget(args.Target); rerr != nil {
		return nil, invalidParams("pipe_pane: %s", rerr.Message)
	}
	// Defensive trim: an entirely-whitespace `shell_command` is never a
	// useful pipeline and tmux would happily run /bin/sh on a blank line.
	// Reject it as a malformed argument so operators see a clean error
	// instead of a silent "pipe started but does nothing" outcome.
	if args.ShellCommand != "" && strings.TrimSpace(args.ShellCommand) == "" {
		return nil, invalidParams("pipe_pane: shell_command must not be only whitespace")
	}
	if rerr := validatePipePaneShellCommand(args.ShellCommand); rerr != nil {
		return nil, invalidParams("pipe_pane: %s", rerr.Message)
	}
	if err := t.Ctl.PipePane(
		ctx,
		t.resolvePaneTarget(args.Target),
		args.ShellCommand,
		args.OutputOnly,
		args.AlsoInput,
	); err != nil {
		return nil, internalError(fmt.Errorf("pipe_pane: %w", err))
	}
	return jsonBlock(map[string]any{"piped": true})
}

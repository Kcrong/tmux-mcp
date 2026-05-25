package server

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// maxRunShellCommandLen caps the `command` payload run_shell accepts.
// 4 KiB is generous for any realistic one-shot pipeline (a build
// invocation, a notification curl, a quick `git status`) and small
// enough that a hostile or buggy caller cannot pin tmux's argv parser
// on multi-megabyte input. Matches the maxPipePaneShellCommandLen
// ceiling so the boundary's "free-form shell pipeline" budget stays
// consistent across tools.
const maxRunShellCommandLen = 4096

// runShellToolDefs holds the JSON Schema for the run_shell tool. It is
// appended onto the main toolDefs slice from this file's init() so the
// registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
//
// Mutating in spirit (it executes a shell pipeline on the controller
// host), so it is **not** in the read-only allowlist — operators
// running -read-only must keep it gated away from inspection-only
// agents. The CAUTION callout in docs/tools.md explains the trust
// boundary in detail.
var runShellToolDefs = []map[string]any{
	{
		"name": "run_shell",
		"description": "Execute a one-shot shell command on the tmux server host via " +
			"`tmux run-shell [-b] [-c <start_dir>] [-t <target>] <command>`. Distinct from " +
			"`pipe_pane` (which hooks pane I/O to a long-running shell pipeline) and " +
			"`send_keys` (which types into a pane and lets the running process see the " +
			"input): `run_shell` runs OUTSIDE any pane and returns the captured stdout to the " +
			"caller. `start_dir` (when set) chdir's tmux into that directory before " +
			"exec'ing /bin/sh; `target` (when set) pins which session tmux evaluates " +
			"format strings against. `background=true` adds `-b`: tmux returns " +
			"immediately, the command runs detached, and stdout is discarded — the " +
			"response carries an empty `stdout` regardless of what the command writes. " +
			"CAUTION: this tool runs ARBITRARY shell commands on the controller host; " +
			"the same trust model that gates `pipe_pane` applies — operators must trust " +
			"the agents calling this tool, run the server with `-read-only` to exclude it " +
			"entirely, or use `-allowlist` to gate the surface away from untrusted clients. " +
			"Returns a small JSON ack `{\"stdout\": \"...\"}` on success.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type": "string",
					"description": "Shell pipeline tmux runs via /bin/sh. Capped at 4096 bytes; " +
						"NUL bytes and other ASCII control characters (except tab) are rejected.",
					"maxLength": maxRunShellCommandLen,
				},
				"start_dir": map[string]any{
					"type":        "string",
					"description": "Optional working directory tmux chdir's into before exec'ing /bin/sh (`-c`).",
				},
				"target": map[string]any{
					"type":        "string",
					"description": "Optional pane/session target (\"session\", \"session:window\", or \"session:window.pane\") that tmux uses for format-string evaluation (`-t`).",
				},
				"background": map[string]any{
					"type":        "boolean",
					"description": "When true, run the command detached (`-b`). tmux returns immediately and stdout is discarded; the response carries an empty `stdout`.",
					"default":     false,
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register run_shell onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, runShellToolDefs...)
}

// validateRunShellCommand enforces the boundary policy for the required
// `command` argument. Empty is rejected; a non-empty value must be
// valid UTF-8, fit under the 4 KiB ceiling, and contain no NUL bytes
// or other ASCII control characters (HT/0x09 is allowed because
// real-world shell lines occasionally use it for spacing, but
// everything else in 0x00..0x1F and 0x7F is rejected up front so a
// stray null/escape can't break the JSON-RPC frame or smuggle past
// tmux's argv parser).
func validateRunShellCommand(cmd string) *rpcError {
	if cmd == "" {
		return invalidParams("command required")
	}
	if len(cmd) > maxRunShellCommandLen {
		return invalidParams("command length %d exceeds %d",
			len(cmd), maxRunShellCommandLen)
	}
	if !utf8.ValidString(cmd) {
		return invalidParams("command must be valid UTF-8")
	}
	for i, r := range cmd {
		if r == 0 {
			return invalidParams("command must not contain NUL bytes (offset %d)", i)
		}
		// Allow horizontal tab (0x09) for spacing in real-world
		// pipelines; reject every other ASCII control char
		// (0x00..0x1F, 0x7F) so a stray newline / ESC can't break
		// the JSON-RPC frame or sneak past tmux's argv parser.
		// Higher Unicode codepoints (e.g. CJK) pass through
		// unchanged — tmux happily forwards them to /bin/sh.
		if r != '\t' && (r < 0x20 || r == 0x7F) {
			return invalidParams("command must not contain control character %#02x (offset %d)", r, i)
		}
	}
	return nil
}

// runShell drives tmuxctl.Controller.RunShell. The handler validates
// the required `command` payload and the optional `target` shape up
// front so a caller passing a malformed value sees CodeInvalidParams
// (-32602) before any tmux command runs. The response is a JSON ack
// `{"stdout": "<captured>"}`; the boundary deliberately wraps the
// captured payload in JSON (rather than returning it as a bare text
// block) so a future field — exit code, duration, etc. — can be added
// without breaking the existing wire shape.
//
// Mutating: run_shell executes a shell pipeline on the controller
// host. The COMMAND ITSELF IS NOT SANDBOXED — operators must trust
// the agents calling this tool. The -allowlist flag is the documented
// way to gate it away from untrusted clients, and -read-only excludes
// it entirely.
func (t *Tools) runShell(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Command    string `json:"command"`
		StartDir   string `json:"start_dir"`
		Target     string `json:"target"`
		Background bool   `json:"background"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("run_shell: %v", err)
	}
	if rerr := validateRunShellCommand(args.Command); rerr != nil {
		return nil, invalidParams("run_shell: %s", rerr.Message)
	}
	if args.Target != "" {
		if rerr := validatePaneTarget(args.Target); rerr != nil {
			return nil, invalidParams("run_shell: %s", rerr.Message)
		}
	}
	if args.StartDir != "" {
		if rerr := validateCwd(args.StartDir); rerr != nil {
			return nil, invalidParams("run_shell: start_dir: %s", rerr.Message)
		}
	}
	out, err := t.Ctl.RunShell(
		ctx,
		args.Command,
		args.StartDir,
		t.resolvePaneTarget(args.Target),
		args.Background,
	)
	if err != nil {
		return nil, internalError(fmt.Errorf("run_shell: %w", err))
	}
	return jsonBlock(map[string]any{"stdout": out})
}

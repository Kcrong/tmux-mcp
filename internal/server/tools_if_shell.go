package server

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// maxIfShellCommandLen caps every free-form string argument the
// if_shell tool accepts. 4 KiB is generous for any realistic
// "if pgrep && wc -l ... ; then display-message ..." pipeline and small
// enough that a hostile or buggy caller cannot pin the JSON-RPC writer
// copying multi-megabyte argv into tmux's command socket. Matches the
// pipe_pane / run_shell ceilings so the boundary's "free-form string"
// budget stays consistent across tools.
const maxIfShellCommandLen = 4096

// ifShellToolDefs holds the JSON Schema for the if_shell tool. It is
// appended onto the main toolDefs slice from this file's init() so the
// registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
//
// Mutating in spirit (it spawns a shell pipeline on the controller host
// AND dispatches a tmux command), so it is **not** in the read-only
// allowlist — operators running -read-only must keep it gated away from
// inspection-only agents.
var ifShellToolDefs = []map[string]any{
	{
		"name": "if_shell",
		"description": "Conditional dispatch via `tmux if-shell [-bF] SHELL_COMMAND TMUX_COMMAND " +
			"[ELSE_TMUX_COMMAND]`. tmux runs `shell_command` through `/bin/sh -c` (or evaluates " +
			"it as a `#{format}` expression when `format_expand=true`); on success (exit 0 or " +
			"non-empty/non-zero expansion) tmux dispatches `then_command`, otherwise it " +
			"dispatches `else_command` (when set). The canonical agent pattern is " +
			"\"if a process is running, do X; else Y\" — e.g. `pgrep -x build-watch && wc -l " +
			"build.log` deciding between `display-message running` and `display-message stopped`. " +
			"`background=true` adds `-b` so tmux runs the shell command detached and the call " +
			"returns immediately (the chosen branch fires later, when the shell exits). " +
			"`format_expand=true` adds `-F` so tmux interprets `shell_command` as a `#{format}` " +
			"expression instead of a shell pipeline. CAUTION: tmux runs `shell_command` via " +
			"`/bin/sh -c`; the command itself is not sandboxed by this server, so operators " +
			"must trust the agents that can call this tool. Use `-allowlist` to gate the " +
			"surface away from untrusted clients. Returns a small JSON ack `{\"dispatched\": " +
			"true}` on success.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"shell_command": map[string]any{
					"type": "string",
					"description": "Shell pipeline tmux runs via /bin/sh -c, or — when format_expand=true — a " +
						"tmux #{format} expression. Capped at 4096 bytes; NUL bytes and other ASCII " +
						"control characters (except tab) are rejected.",
					"maxLength": maxIfShellCommandLen,
				},
				"then_command": map[string]any{
					"type": "string",
					"description": "tmux command line dispatched on success (exit 0 / non-empty expansion). " +
						"Same length / control-char policy as shell_command.",
					"maxLength": maxIfShellCommandLen,
				},
				"else_command": map[string]any{
					"type": "string",
					"description": "Optional tmux command line dispatched on failure. Same length / control-char " +
						"policy as shell_command. Omit (or pass an empty string) to make the failure " +
						"branch a no-op.",
					"maxLength": maxIfShellCommandLen,
				},
				"background": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, runs `shell_command` detached (`-b`); the call returns immediately and tmux dispatches the chosen branch when the shell exits.",
				},
				"format_expand": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, treat `shell_command` as a tmux `#{format}` expression (`-F`) instead of running it through /bin/sh.",
				},
			},
			"required":             []string{"shell_command", "then_command"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register if_shell onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, ifShellToolDefs...)
}

// validateIfShellCommand enforces the boundary policy for a free-form
// command argument (shell_command, then_command, else_command). Empty
// is allowed only at the top level for else_command — the per-field
// "required" check fires before this helper for the other two — so
// this helper itself just enforces "if non-empty, the bytes are
// well-formed". A non-empty value must be valid UTF-8, fit under the
// 4 KiB ceiling, and contain no NUL bytes or other ASCII control
// characters (HT/0x09 is allowed because real-world shell lines
// occasionally use it, but everything else in 0x00..0x1F and 0x7F is
// rejected up front so a stray null/escape can't break the JSON-RPC
// frame or smuggle past tmux's argv parser).
func validateIfShellCommand(field, cmd string) *rpcError {
	if cmd == "" {
		return nil
	}
	if len(cmd) > maxIfShellCommandLen {
		return invalidParams("%s length %d exceeds %d",
			field, len(cmd), maxIfShellCommandLen)
	}
	if !utf8.ValidString(cmd) {
		return invalidParams("%s must be valid UTF-8", field)
	}
	for i, r := range cmd {
		if r == 0 {
			return invalidParams("%s must not contain NUL bytes (offset %d)", field, i)
		}
		// Allow horizontal tab (0x09) for spacing in real-world
		// pipelines; reject every other ASCII control char (0x00..0x1F,
		// 0x7F) so a stray newline / ESC can't break the JSON-RPC frame
		// or sneak past tmux's argv parser. Higher Unicode codepoints
		// (e.g. CJK) pass through unchanged — tmux happily forwards
		// them to /bin/sh.
		if r != '\t' && (r < 0x20 || r == 0x7F) {
			return invalidParams("%s must not contain control character %#02x (offset %d)", field, r, i)
		}
	}
	return nil
}

// ifShell drives tmuxctl.Controller.IfShell. The handler validates the
// required `shell_command` and `then_command` payloads up front, plus
// the optional `else_command` when present, so a caller passing a
// malformed value sees CodeInvalidParams (-32602) before any tmux
// command runs. The response is a small JSON ack `{"dispatched":
// true}`; the boundary deliberately does not echo the resolved tmux
// commands because tmux gives no useful confirmation back, and a
// follow-up `display_message`/`capture` is the natural way to confirm
// the chosen branch ran.
//
// Mutating: if_shell spawns a shell pipeline on the controller host
// AND dispatches a tmux command on the success/failure branch. The
// COMMAND ITSELF IS NOT SANDBOXED — operators must trust the agents
// calling this tool. The -allowlist flag is the documented way to gate
// it away from untrusted clients.
//
// Errors: tmux's "syntax error" / "unknown command" diagnostics flow
// back as CodeInternal (-32603) — they signal a legitimate caller bug
// the agent wants to debug. Unlike the pane-targeted tools, if-shell
// does not take `-t`, so there is no session-not-found mapping here.
func (t *Tools) ifShell(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		ShellCommand string `json:"shell_command"`
		ThenCommand  string `json:"then_command"`
		ElseCommand  string `json:"else_command"`
		Background   bool   `json:"background"`
		FormatExpand bool   `json:"format_expand"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("if_shell: %v", err)
	}
	if args.ShellCommand == "" {
		return nil, invalidParams("if_shell: shell_command required")
	}
	if args.ThenCommand == "" {
		return nil, invalidParams("if_shell: then_command required")
	}
	if rerr := validateIfShellCommand("shell_command", args.ShellCommand); rerr != nil {
		return nil, invalidParams("if_shell: %s", rerr.Message)
	}
	if rerr := validateIfShellCommand("then_command", args.ThenCommand); rerr != nil {
		return nil, invalidParams("if_shell: %s", rerr.Message)
	}
	if rerr := validateIfShellCommand("else_command", args.ElseCommand); rerr != nil {
		return nil, invalidParams("if_shell: %s", rerr.Message)
	}
	if err := t.Ctl.IfShell(
		ctx,
		args.ShellCommand,
		args.ThenCommand,
		args.ElseCommand,
		args.Background,
		args.FormatExpand,
	); err != nil {
		return nil, internalError(fmt.Errorf("if_shell: %w", err))
	}
	return jsonBlock(map[string]any{"dispatched": true})
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// maxConfirmPromptLen caps the up-front length check applied to the
// optional `prompt` argument. tmux happily accepts long prompts but a
// realistic y/n confirmation message rarely exceeds a few dozen
// characters; 128 bytes leaves comfortable headroom for translated
// strings while still bounding the JSON-RPC payload size against a
// hostile caller pasting an unbounded string.
const maxConfirmPromptLen = 128

// maxConfirmTargetLen caps the optional `target` (client) argument.
// tmux client names are TTY paths (e.g. "/dev/pts/3", "/dev/ttys001"),
// well under 64 bytes in practice; 128 matches maxConfirmPromptLen
// so the boundary keeps a uniform per-field ceiling.
const maxConfirmTargetLen = 128

// maxConfirmCommandLen caps the REQUIRED `command` argument. tmux
// commands can chain via ";" and embed quoted arguments, so this
// field is intentionally generous (4 KiB) to cover real-world
// destructive command shapes (e.g. `kill-session -t foo \; new-session
// -d -s bar`) without admitting an unbounded blob.
const maxConfirmCommandLen = 4096

// confirmTargetRE accepts the conservative shape a tmux client target
// can take: alphanumerics, slashes, dots, dashes, and underscores —
// the alphabet legitimate TTY paths (`/dev/pts/3`, `/dev/ttys001`,
// `/dev/tty.usbserial-1410`) use plus the rare relative-name form
// some operators alias their clients to. We deliberately do NOT
// accept whitespace, shell metachars, or backslashes — none of those
// appear in legitimate client targets and admitting them would risk
// stray quoting / argv-injection if a future tmux version starts
// treating any of them specially in `-t <client>`.
var confirmTargetRE = regexp.MustCompile(`^[A-Za-z0-9/_.\-]+$`)

// validateConfirmTarget enforces the conservative client-target
// policy on confirm_before's optional `target` argument. Empty is
// allowed (the controller asks tmux to use the caller's current
// client, which on a headless server surfaces as the typed
// ErrSessionNotFound sentinel — there is no client to ask); a
// non-empty value must satisfy the regex/length policy so a stray
// quote or path-injection attempt cannot slip through to tmux's argv.
func validateConfirmTarget(target string) *rpcError {
	if target == "" {
		return nil
	}
	if len(target) > maxConfirmTargetLen {
		return invalidParams("target length %d out of range [1..%d]", len(target), maxConfirmTargetLen)
	}
	if !confirmTargetRE.MatchString(target) {
		return invalidParams("target %q must match %s", target, confirmTargetRE.String())
	}
	return nil
}

// confirmBeforeToolDefs holds the JSON Schema for the confirm_before
// tool. It is appended onto the main toolDefs slice via the package
// init() in this file so the registration site stays close to the
// handler — the dispatcher in tools.go only needs the single name →
// handler entry.
var confirmBeforeToolDefs = []map[string]any{
	{
		"name": "confirm_before",
		"description": "Stage a tmux command behind an interactive y/n prompt via " +
			"`tmux confirm-before [-p prompt] [-t target-client] command`. tmux pops " +
			"a confirmation prompt up in the matching client and only runs `command` " +
			"if the user accepts — the controller surfaces this as a single fire-and-" +
			"forget call so an agent can stage destructive ops without making the tmux " +
			"UI silently auto-execute. Returns `{\"ack\": true, \"prompt\": \"<text>\"}` " +
			"on a successful queue. On a headless server with nothing attached the call " +
			"surfaces -32000 (CodeSessionNotFound) — there is no client to ask, so the " +
			"contract is deliberately NOT idempotent.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   maxConfirmCommandLen,
					"description": "tmux command to run if the user accepts (REQUIRED). Free-form (capped at 4096 chars).",
				},
				"prompt": map[string]any{
					"type":        "string",
					"maxLength":   maxConfirmPromptLen,
					"description": "Optional y/n prompt text; tmux falls back to \"Confirm 'CMD'? (y/n)\" when omitted.",
				},
				"target": map[string]any{
					"type":        "string",
					"maxLength":   maxConfirmTargetLen,
					"description": "Optional tmux client target (typically a TTY path like \"/dev/pts/0\"); regex `^[A-Za-z0-9/_.\\-]+$`. Omit to use the caller's current client.",
				},
			},
			"required": []string{"command"},
			// Lock the schema so a typo'd field (e.g. "cmd" or
			// "client") fails fast with -32602 instead of being
			// silently ignored.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register confirm_before onto the main toolDefs slice. Doing
	// this from init() keeps the new tool surface entirely contained
	// in this file (apart from the single dispatcher case in
	// tools.go) and avoids touching the shared toolDefs literal that
	// other PRs are editing.
	toolDefs = append(toolDefs, confirmBeforeToolDefs...)
}

// confirmBefore drives tmuxctl.Controller.ConfirmBefore and
// serialises the result to the standard
// `{"content":[{"type":"text","text":"<json>"}]}` envelope MCP
// expects from a tools/call. The response shape is a flat object so
// future additions (e.g. an echo of the resolved target client) can
// land alongside without breaking callers that only read the
// boolean.
//
// Argument handling:
//   - `command` is REQUIRED; tmux refuses confirm-before without
//     one and so does the boundary, with a -32602 surface so the
//     caller learns from a clear schema error rather than a buried
//     tmux usage line.
//   - `prompt` is optional; tmux falls back to its default phrasing
//     when empty, so the controller omits `-p` rather than passing
//     an empty argument.
//   - `target` is optional; when present it must satisfy the
//     conservative regex/length policy so a stray quote or path-
//     injection attempt cannot slip through to tmux's argv.
//
// Error mapping:
//   - malformed args / unknown field / missing command → -32602
//     (CodeInvalidParams).
//   - headless server / named client does not exist → -32000
//     (CodeSessionNotFound), via the wrapped errs.ErrSessionNotFound
//     the controller emits.
//   - any other tmux failure → -32603 (CodeInternal).
//
// This is a MUTATING tool (it queues a destructive command behind a
// y/n prompt; the user's acceptance runs whatever was passed in
// `command`), so it is deliberately NOT in readOnlyTools — a
// -read-only deployment must reject it before the handler runs.
func (t *Tools) confirmBefore(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Command string `json:"command"`
		Prompt  string `json:"prompt"`
		Target  string `json:"target"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("confirm_before: %v", err)
	}
	if args.Command == "" {
		return nil, invalidParams("command: required")
	}
	if len(args.Command) > maxConfirmCommandLen {
		return nil, invalidParams("command length %d exceeds %d", len(args.Command), maxConfirmCommandLen)
	}
	if len(args.Prompt) > maxConfirmPromptLen {
		return nil, invalidParams("prompt length %d exceeds %d", len(args.Prompt), maxConfirmPromptLen)
	}
	if rerr := validateConfirmTarget(args.Target); rerr != nil {
		return nil, rerr
	}
	if err := t.Ctl.ConfirmBefore(ctx, args.Prompt, args.Target, args.Command); err != nil {
		return nil, internalError(fmt.Errorf("confirm_before: %w", err))
	}
	return jsonBlock(map[string]any{"ack": true, "prompt": args.Prompt})
}

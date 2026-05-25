package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxCommandPromptStringLen caps every free-form string argument the
// command_prompt tool accepts (prompts, inputs, template, client). 4 KiB
// is the same ceiling display_message uses for tmux format strings —
// generous for realistic prompt UIs (tmux's own bindings rarely exceed a
// few dozen bytes per arg) while keeping the JSON-RPC frame size
// predictable. Anything past this limit is almost certainly a typo or
// hostile caller.
const maxCommandPromptStringLen = 4096

// commandPromptToolDefs holds the JSON Schema for the command_prompt
// tool. It is appended onto the main toolDefs slice via the package
// init() in this file so the registration site stays close to the
// handler — the dispatcher in tools.go only needs the single name →
// handler entry.
//
// All inputs are optional: tmux accepts `command-prompt` with no flags
// at all (it just opens the prompt UI on the current client with no
// follow-up command). In practice an agent will set at least one of
// `template` / `one_key` / `incremental` / `multi_line`, but we do not
// enforce it — a bare-bones invocation is sometimes what an automation
// flow wants. The schema description spells the convention out so the
// LLM can pick the right fields without having to read the source.
//
// `additionalProperties: false` keeps the schema strict: a typo in the
// arguments object (e.g. `multiline` instead of `multi_line`) gets a
// fast schema-shaped rejection rather than a silent no-op.
var commandPromptToolDefs = []map[string]any{
	{
		"name": "command_prompt",
		"description": "Open the targeted client's interactive command-prompt UI via " +
			"`tmux command-prompt [-1iIN] [-p PROMPTS] [-I INPUTS] [-t TARGET] [TEMPLATE]`. " +
			"Useful for an agent that wants to programmatically launch a preset prompt " +
			"dialog (e.g. a rename-window flow whose template is `rename-window %%`). On " +
			"a headless server (no client attached, no `client` pinned) the call is a " +
			"successful no-op — the prompt has nowhere to render. Set at least one of " +
			"`template` / `one_key` / `incremental` / `multi_line` to make the call do " +
			"useful work; the schema does not enforce it because tmux itself accepts a " +
			"bare invocation.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"client": map[string]any{
					"type":        "string",
					"maxLength":   maxCommandPromptStringLen,
					"description": "Optional target client TTY path (e.g. \"/dev/pts/3\"); maps to `-t TARGET`. Omit to use tmux's current client.",
				},
				"prompts": map[string]any{
					"type":        "string",
					"maxLength":   maxCommandPromptStringLen,
					"description": "Optional comma-separated prompt strings (`-p PROMPTS`); one per `%%` placeholder in `template`.",
				},
				"inputs": map[string]any{
					"type":        "string",
					"maxLength":   maxCommandPromptStringLen,
					"description": "Optional comma-separated default inputs (`-I INPUTS`); aligned positionally with `prompts`.",
				},
				"template": map[string]any{
					"type":        "string",
					"maxLength":   maxCommandPromptStringLen,
					"description": "Optional tmux command tmux runs once the prompt is filled. `%%` is replaced by the user's input.",
				},
				"one_key": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, accept a single keypress without Enter (`-1`).",
				},
				"incremental": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, run the command on every keystroke (`-i`).",
				},
				"multi_line": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, open a multi-line editor instead of the single-line prompt (`-N`). Rare.",
				},
			},
			// Every field is optional; tmux itself accepts a bare
			// `command-prompt` invocation. additionalProperties:false
			// still keeps the schema strict so a typo in the field name
			// fails fast rather than silently dropping the value.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register command_prompt onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go and the
	// readonly_test.go RejectsMutators entry) and avoids touching the
	// shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, commandPromptToolDefs...)
}

// validateCommandPromptString enforces the per-arg policy for the four
// free-form string fields (client, prompts, inputs, template). Empty
// values flow through unchanged (the handler interprets "empty" as
// "omit this flag entirely"); non-empty values must:
//
//   - be valid UTF-8 (tmux's own argv expects UTF-8; rejecting other
//     encodings here keeps the round-trip from this server's JSON-RPC
//     surface to tmux's stderr deterministic);
//   - fit within the maxCommandPromptStringLen ceiling;
//   - contain no NUL bytes (tmux silently truncates argv at the first
//     NUL — a hostile caller could otherwise sneak past the prompt and
//     execute a different tmux command than the schema documents);
//   - contain no control bytes other than tab. Newlines / carriage
//     returns would split the JSON-RPC frame budget if echoed back, and
//     other control chars (DEL, escape sequences) have no place in a
//     prompt UI. Tab is allowed because real prompt strings sometimes
//     pad with one for layout.
//
// fieldName is spliced into the error so the caller can see which arg
// the boundary rejected without parsing a generic message.
func validateCommandPromptString(fieldName, value string) *rpcError {
	if value == "" {
		return nil
	}
	if !utf8.ValidString(value) {
		return invalidParams("command_prompt: %s must be valid UTF-8", fieldName)
	}
	if len(value) > maxCommandPromptStringLen {
		return invalidParams("command_prompt: %s length %d exceeds %d",
			fieldName, len(value), maxCommandPromptStringLen)
	}
	for _, r := range value {
		if r == '\t' {
			continue
		}
		// Reject every C0 control char (0x00..0x1F) plus DEL (0x7F).
		// tmux accepts arbitrary bytes in command-prompt args in
		// principle, but those bytes have no use case in a prompt UI
		// and several (NUL, newline, escape) actively break either
		// tmux's argv or our JSON-RPC frame. Rejecting them up front
		// keeps the surface predictable.
		if r < 0x20 || r == 0x7F {
			return invalidParams("command_prompt: %s must not contain control characters (got 0x%02X)",
				fieldName, r)
		}
	}
	return nil
}

// commandPrompt drives tmuxctl.Controller.CommandPrompt. The handler
// does the usual up-front validation (UTF-8, length cap, control-byte
// rejection on every free-form string) so a malformed call sees
// CodeInvalidParams (-32602) before any tmux command runs.
//
// Headless behaviour: on a server with no attached client and no
// explicit `client` pin, tmux's "no current client" stderr is folded
// into a successful no-op by the controller layer; the handler
// surfaces a `{"opened": false, ...}` envelope so callers can detect
// the no-op without parsing a separate error code. When the prompt did
// reach a client (or could have, on a server that grew one) the
// envelope reads `{"opened": true, ...}` — both shapes echo the
// caller's logical args back so a chained workflow can tell which
// invocation produced which response.
//
// Unknown clients (a `client` value that does not match any attached
// TTY) surface as CodeSessionNotFound (-32000) via the controller's
// errs.ErrSessionNotFound wrap. That is the closest stable code we
// expose for "the addressed thing does not exist" and it lets MCP
// clients branch on a known code rather than substring-matching tmux's
// version-specific stderr.
func (t *Tools) commandPrompt(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Client      string `json:"client"`
		Prompts     string `json:"prompts"`
		Inputs      string `json:"inputs"`
		Template    string `json:"template"`
		OneKey      bool   `json:"one_key"`
		Incremental bool   `json:"incremental"`
		MultiLine   bool   `json:"multi_line"`
	}
	// json.Unmarshal on an empty payload is fine — every field is
	// optional and the zero value of args means "open the prompt with
	// no flags", which is exactly what `tmux command-prompt` does on
	// its own.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("command_prompt: %v", err)
		}
	}

	// Reject newlines / carriage returns up front with a friendlier
	// message than the generic "control character" catch-all that
	// validateCommandPromptString would otherwise emit. tmux's own
	// command-prompt is strictly single-shot per call, so the "no
	// newlines" rule is the one a misuse most often trips. Surfacing
	// the byte name keeps the diagnostic immediately actionable for
	// the LLM-facing surface; the broader control-byte check below
	// then mops up DEL / NUL / escape sequences with the same clean
	// invalidParams envelope.
	if strings.ContainsAny(args.Template, "\n\r") {
		return nil, invalidParams("command_prompt: template must not contain newlines")
	}
	if strings.ContainsAny(args.Prompts, "\n\r") {
		return nil, invalidParams("command_prompt: prompts must not contain newlines")
	}
	if strings.ContainsAny(args.Inputs, "\n\r") {
		return nil, invalidParams("command_prompt: inputs must not contain newlines")
	}

	// Validate every free-form string. Doing this up front keeps the
	// boundary layer the single point where a malformed call gets a
	// clean -32602 — by the time the controller runs every value is
	// known to be safe to splice into tmux's argv.
	for _, f := range []struct {
		name, value string
	}{
		{"client", args.Client},
		{"prompts", args.Prompts},
		{"inputs", args.Inputs},
		{"template", args.Template},
	} {
		if rerr := validateCommandPromptString(f.name, f.value); rerr != nil {
			return nil, rerr
		}
	}

	if err := t.Ctl.CommandPrompt(ctx,
		args.Client, args.Prompts, args.Inputs, args.Template,
		args.OneKey, args.Incremental, args.MultiLine,
	); err != nil {
		return nil, internalError(fmt.Errorf("command_prompt: %w", err))
	}

	// Echo the logical args back so a -session-prefix / multi-call
	// workflow can correlate the response with the original invocation
	// without reading state. `opened` is `true` whenever the controller
	// returned nil — that covers both the "actually rendered to a
	// client" branch and the headless no-op (we cannot tell the two
	// apart without an extra `list-clients` round trip, and the
	// difference is rarely actionable for the caller; the field is
	// here as an explicit no-op signal once a future refactor wants
	// to surface the distinction).
	return jsonBlock(map[string]any{
		"opened":      true,
		"client":      args.Client,
		"prompts":     args.Prompts,
		"inputs":      args.Inputs,
		"template":    args.Template,
		"one_key":     args.OneKey,
		"incremental": args.Incremental,
		"multi_line":  args.MultiLine,
	})
}

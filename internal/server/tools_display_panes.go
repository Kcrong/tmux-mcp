package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// displayPanesMaxClientNameLen caps the up-front length check applied
// to the optional `target` argument on display_panes. tmux client
// names are TTY paths (e.g. "/dev/pts/3", "/dev/ttys001") — typically
// well under 64 bytes — so a 256-byte ceiling leaves comfortable
// headroom for unusual platforms while still bounding the JSON-RPC
// payload size against a hostile caller pasting an unbounded string.
// Mirrors the cap detach_client / refresh_client / lock_client use;
// sized identically so a future rebase that consolidates the
// validators sees no drift.
const displayPanesMaxClientNameLen = 256

// displayPanesMaxTemplateLen caps the optional `template` argument
// passed to display_panes. tmux templates are typically a single short
// command like `select-pane -t %%`; anything past 4 KiB is almost
// certainly a buggy or hostile caller, and bounding it here keeps the
// JSON-RPC frame size predictable regardless of upstream behaviour.
// Mirrors maxPaneCommandLen (used by pane_split's `command`) so a
// future refactor that consolidates the caps sees no drift.
const displayPanesMaxTemplateLen = 4096

// displayPanesClientNameRE accepts the conservative shape a tmux
// client name can take in practice: an absolute path under /dev/,
// optionally with alnum / underscore / dash / dot / colon characters
// that show up in real-world TTY paths (`/dev/pts/3`, `/dev/ttys001`,
// `/dev/tty.usbserial-1410`). We deliberately do NOT accept
// whitespace, shell metachars, glob characters, or backslashes — none
// of those appear in legitimate TTY paths and admitting them would
// risk stray quoting / argv-injection if a future tmux version starts
// treating any of them specially in `-t <client>`.
//
// Sibling regex of detach_client / refresh_client / lock_client;
// intentionally duplicated here so this file builds standalone against
// origin/main today and so a rebase that lands one of the other
// client tools first can dedupe in one place.
var displayPanesClientNameRE = regexp.MustCompile(`^/[A-Za-z0-9_./:-]+$`)

// validateDisplayPanesTarget enforces the conservative client-name
// policy for the optional `target` argument on display_panes. Empty is
// allowed (the call resolves against the caller's "current" client,
// which on a headless server is folded onto a successful no-op
// downstream); a non-empty value must satisfy the regex/length policy
// so a stray quote or path-injection attempt cannot slip through to
// tmux's argv.
func validateDisplayPanesTarget(target string) *rpcError {
	if target == "" {
		return nil
	}
	if len(target) > displayPanesMaxClientNameLen {
		return invalidParams("display_panes: target length %d out of range [1..%d]",
			len(target), displayPanesMaxClientNameLen)
	}
	if !displayPanesClientNameRE.MatchString(target) {
		return invalidParams("display_panes: target %q must match %s",
			target, displayPanesClientNameRE.String())
	}
	return nil
}

// validateDisplayPanesTemplate enforces the length cap on the optional
// `template` argument. We deliberately do NOT validate the template
// body — tmux's format grammar (the `%%` and `#{...}` substitutions)
// is large and version-dependent, so any character-level allowlist
// would either ban legitimate templates or admit malformed ones. The
// length cap alone is enough to keep the JSON-RPC frame predictable;
// tmux itself handles malformed format strings by failing the
// substitution at runtime, which surfaces cleanly through run().
func validateDisplayPanesTemplate(tpl string) *rpcError {
	if tpl == "" {
		return nil
	}
	if len(tpl) > displayPanesMaxTemplateLen {
		return invalidParams("display_panes: template length %d out of range [1..%d]",
			len(tpl), displayPanesMaxTemplateLen)
	}
	return nil
}

// displayPanesToolDefs holds the JSON Schema for the display_panes
// tool. It is appended onto the main toolDefs slice via the package
// init() in this file so the registration site stays close to the
// handler — the dispatcher in tools.go only needs the single
// name → handler entry.
var displayPanesToolDefs = []map[string]any{
	{
		"name": "display_panes",
		"description": "Briefly draw each pane's identifier overlay on a tmux client via " +
			"`tmux display-panes [-b] [-d duration] [-N] [-t CLIENT] [template]`. " +
			"This is the visual primitive that lets a human pick a pane by index; " +
			"agents can use it to surface the picker on an attached terminal so a " +
			"user can choose, or to fire a templated tmux command keyed off the " +
			"selection. Pass `target` (the path-like client name shown in " +
			"`list_clients`, e.g. \"/dev/pts/0\") to draw on a specific client; " +
			"omit it to draw on the caller's current client. Pass `block=true` to " +
			"wait until the user finishes selecting before returning (otherwise the " +
			"call returns as soon as the picker is drawn). Pass `duration_ms` to " +
			"override how long the overlay stays painted; 0 falls back to tmux's " +
			"`display-panes-time` (typically 1000ms). Pass `no_prefix=true` to free " +
			"the prefix key during the picker (`-N`). Pass `template` (e.g. " +
			"\"select-pane -t %%\") to run a tmux command against the selected pane; " +
			"omit it to leave tmux's default behaviour. Returns `{\"displayed\": true}` " +
			"on success. Headless servers with nothing attached are a successful " +
			"no-op — the boundary swallows tmux's \"no current client\" stderr so " +
			"callers can fire-and-forget without a separate `list_clients` round-trip. " +
			"This is a MUTATING tool (it draws onto a live client / can run a templated " +
			"command), so a `-read-only` deployment rejects it before the handler runs.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"block": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, pass `-b` so tmux waits until the user has finished selecting before returning.",
				},
				"duration_ms": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"maximum":     maxDurationMs,
					"default":     0,
					"description": "How long to paint the overlay, in milliseconds; maps to `-d`. 0 falls back to tmux's `display-panes-time` default.",
				},
				"no_prefix": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, pass `-N` so the prefix key is not reserved during the picker.",
				},
				"target": map[string]any{
					"type":        "string",
					"maxLength":   displayPanesMaxClientNameLen,
					"description": "Optional tmux client name (typically a TTY path like \"/dev/pts/0\"); regex `^/[A-Za-z0-9_./:-]+$`. Maps to `-t CLIENT`.",
				},
				"template": map[string]any{
					"type":        "string",
					"maxLength":   displayPanesMaxTemplateLen,
					"description": "Optional tmux command template run against the selected pane (e.g. \"select-pane -t %%\"). Forwarded verbatim; tmux substitutes `%%` (and other format tokens) at execution time.",
				},
			},
			// Lock the schema so a typo'd field (e.g. "duration",
			// "client") fails fast with -32602 instead of silently
			// being ignored.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register display_panes onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, displayPanesToolDefs...)
}

// displayPanes drives [tmuxctl.Controller.DisplayPanes] and serialises
// the result to the standard
// `{"content":[{"type":"text","text":"<json>"}]}` envelope MCP expects
// from a tools/call. The response shape is a flat object keyed by
// "displayed" so a future addition (e.g. a count of the number of
// panes shown) can land alongside without breaking callers that only
// read the boolean.
//
// Argument handling:
//   - `block` (optional, default false) → `-b`.
//   - `duration_ms` (optional, default 0) → `-d <ms>` when non-zero;
//     0 leaves the flag off (tmux's display-panes-time default applies).
//     The standard validateDurationMs cap (10 minutes) keeps a hostile
//     caller from pinning a client in the picker for unbounded durations.
//   - `no_prefix` (optional, default false) → `-N`.
//   - `target` (optional) → `-t <client>`. When present it must
//     satisfy the conservative TTY-path regex / length policy so a
//     stray quote or path-injection attempt cannot slip through to
//     tmux's argv.
//   - `template` (optional) → trailing positional arg. Length-capped
//     up front so a hostile template cannot inflate the argv; the
//     body is forwarded verbatim because tmux's format grammar is too
//     large to validate character-by-character.
//
// Error mapping:
//   - malformed args / unknown field / out-of-range duration / bad
//     target / oversized template → -32602 (invalid params).
//   - named target client does not exist → -32000 (CodeSessionNotFound),
//     via the wrapped errs.ErrSessionNotFound the controller emits.
//   - any other tmux failure → -32603 (internal).
//
// This is a MUTATING tool (it draws onto a live client / can fire a
// templated tmux command on selection), so it is deliberately NOT in
// readOnlyTools — a -read-only deployment must reject it before the
// handler runs.
func (t *Tools) displayPanes(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Block      bool   `json:"block"`
		DurationMs int    `json:"duration_ms"`
		NoPrefix   bool   `json:"no_prefix"`
		Target     string `json:"target"`
		Template   string `json:"template"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}`, in which case the call resolves to `tmux
	// display-panes` with no flags, which on a headless server is the
	// expected fire-and-forget no-op. Bare `tools/call` with no
	// arguments object hits this branch with len(raw)==0 too.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("display_panes: %v", err)
		}
	}
	if rerr := validateDurationMs("duration_ms", args.DurationMs); rerr != nil {
		return nil, rerr
	}
	if rerr := validateDisplayPanesTarget(args.Target); rerr != nil {
		return nil, rerr
	}
	if rerr := validateDisplayPanesTemplate(args.Template); rerr != nil {
		return nil, rerr
	}
	if err := t.Ctl.DisplayPanes(ctx, tmuxctl.DisplayPanesOpts{
		Block:    args.Block,
		Duration: time.Duration(args.DurationMs) * time.Millisecond,
		NoPrefix: args.NoPrefix,
		Target:   args.Target,
		Template: args.Template,
	}); err != nil {
		return nil, internalError(fmt.Errorf("display_panes: %w", err))
	}
	return jsonBlock(map[string]any{"displayed": true})
}

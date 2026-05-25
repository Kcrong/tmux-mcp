package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxSetWindowOptionNameLen caps the option-name length the boundary
// will forward to tmux. Real tmux option names are well under 64 bytes
// (`synchronize-panes`, `automatic-rename`, `pane-border-format`); the
// 64-byte ceiling is generous for every documented option while still
// ruling out a malicious caller asking us to allocate a multi-MB argv
// slot on the parent process.
const maxSetWindowOptionNameLen = 64

// maxSetWindowOptionValueLen caps the value the boundary will forward
// to tmux. Window-option values include format strings (the longest
// stock examples are ~1 KiB) and short scalars (`on` / `off`,
// `vi` / `emacs`); 4 KiB is plenty for every realistic case and small
// enough that a hostile caller cannot stash a 100 MB blob in an argv
// slot the parent process must marshal back through the JSON-RPC
// reply.
const maxSetWindowOptionValueLen = 4096

// setWindowOptionNameRE matches the conservative shape every real
// tmux option name conforms to: a letter followed by letters, digits,
// underscores, or dashes. Deliberately stricter than sessionNameRE
// (which permits a leading digit) because tmux's own option-name
// space starts with a letter — `mode-keys`, `automatic-rename`, etc.
// — and a leading digit would be unusual enough to almost certainly
// indicate a typo or argv-injection attempt.
var setWindowOptionNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// setWindowOptionTargetRE accepts the window-target shapes
// `set-window-option -t TARGET` understands: a bare window name or
// numeric index, the qualified `SESSION:WINDOW` form (with WINDOW
// either a name or a numeric index), or tmux's internal `@N` window
// id. The regex is the cheap up-front guard against stray quoting /
// shell metachars / overly long inputs; tmux itself does the deeper
// "does this window exist?" check after the boundary is satisfied.
//
// Distinct from validateWindowTarget (which the rest of the surface
// uses for the bare-window-component case) because set-window-option's
// `-t` argument typically carries a session prefix —
// `synchronize-panes` only makes sense on a specific window, and the
// `SESSION:WINDOW` form is the canonical way to address it.
var setWindowOptionTargetRE = regexp.MustCompile(`^([A-Za-z0-9_-]+(:[A-Za-z0-9_-]+)?|@[0-9]+)$`)

// setWindowOptionToolDefs holds the JSON Schema for the
// set_window_option tool. It is appended onto the main toolDefs slice
// via this file's init() so the registration site stays close to the
// handler — the dispatcher in tools.go only needs the single
// name → handler entry.
//
// The schema marks `name` required; `value` is required for normal
// sets but suppressed when `unset=true`. Since JSON Schema cannot
// express "value required unless unset", that constraint is enforced
// in the handler (with a CodeInvalidParams response when violated).
// `target` is optional — the tool falls back to the active window
// when global=false and target="" is the empty string, matching the
// tmux CLI's own behaviour, but the handler still requires either
// target or global=true so the call lands somewhere deterministic.
var setWindowOptionToolDefs = []map[string]any{
	{
		"name": "set_window_option",
		"description": "Set or clear a tmux window option via `tmux set-window-option [-aFgoqu] [-t TARGET] OPTION VALUE`. " +
			"Window options live on tmux's per-window table — synchronize-panes, automatic-rename, mode-keys, " +
			"pane-border-format, etc. — distinct from server / session options handled by `set_option`. " +
			"`target` is typically `SESSION:WINDOW` and is required when `global` is false. Pass `global: true` " +
			"to modify the global window-options table (the defaults inherited by every window). " +
			"Pass `unset: true` to clear the override (`-u`); `value` is then ignored and may be omitted. " +
			"Boolean knobs `append` (`-a`, append to a string-list option), `format_expand` (`-F`, run #{format} " +
			"substitutions before storing), and `allow_missing` (`-q`, suppress unknown-option diagnostics) " +
			"map directly to the tmux CLI flags. Schema rejects unknown fields up front.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Option name, e.g. `synchronize-panes`. Regex `^[A-Za-z][A-Za-z0-9_-]*$`, len 1-64.",
				},
				"value": map[string]any{
					"type":        "string",
					"description": "Option value. Required unless `unset=true`. Capped at 4096 bytes; NUL bytes and most control characters are rejected.",
				},
				"target": map[string]any{
					"type":        "string",
					"description": "Window target, typically `SESSION:WINDOW` or `@N`. Required when `global` is false.",
				},
				"append": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, append to an existing string-list option (`-a`).",
				},
				"format_expand": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, run the value through tmux's #{format} substitution (`-F`).",
				},
				"global": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, modify the global window-options table (`-g`).",
				},
				"allow_missing": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, suppress unknown-option diagnostics (`-q`).",
				},
				"unset": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, clear the override (`-u`) instead of setting a value.",
				},
			},
			"required":             []string{"name"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register set_window_option onto the main toolDefs slice. Doing
	// this from init() keeps the new tool surface entirely contained
	// in this file (apart from the single dispatcher case in tools.go)
	// and avoids touching the shared toolDefs literal that other PRs
	// are editing.
	toolDefs = append(toolDefs, setWindowOptionToolDefs...)
}

// maxSetWindowOptionTargetLen bounds the `target` argument. tmux
// targets are at most "session:window" with each component bounded to
// 64 bytes (the per-name policy elsewhere on the surface), plus the
// 1-byte separator — 129 is the natural ceiling. We round up to the
// session-name policy ×2 so a future tmux version that accepts longer
// component names doesn't immediately trip the guard.
const maxSetWindowOptionTargetLen = maxSessionNameLen*2 + 1

// validateSetWindowOptionTarget enforces the up-front guard for the
// `target` argument. The schema marks target optional (it may be
// omitted under global=true), so the empty-string check lives in the
// caller; this helper assumes it is invoked only when a non-empty
// target is required.
func validateSetWindowOptionTarget(target string) *rpcError {
	if target == "" {
		return invalidParams("target required")
	}
	if len(target) > maxSetWindowOptionTargetLen {
		return invalidParams("target length %d out of range [1..%d]", len(target), maxSetWindowOptionTargetLen)
	}
	if !setWindowOptionTargetRE.MatchString(target) {
		return invalidParams("target %q must match %s", target, setWindowOptionTargetRE.String())
	}
	return nil
}

// validateSetWindowOptionName enforces the conservative option-name
// policy: must start with a letter, then letters / digits / underscore
// / dash; bounded to 64 bytes so a malicious caller cannot expand the
// argv. The regex is stricter than sessionNameRE on the leading-char
// rule because tmux's own option-name space always starts with a
// letter.
func validateSetWindowOptionName(name string) *rpcError {
	if name == "" {
		return invalidParams("name required")
	}
	if len(name) > maxSetWindowOptionNameLen {
		return invalidParams("name length %d out of range [1..%d]", len(name), maxSetWindowOptionNameLen)
	}
	if !setWindowOptionNameRE.MatchString(name) {
		return invalidParams("name %q must match %s", name, setWindowOptionNameRE.String())
	}
	return nil
}

// validateSetWindowOptionValue enforces the value-side guards:
// length bound (4 KiB), valid UTF-8, no NUL bytes, and no control
// characters other than tab. Tab is allowed when format_expand=false
// because tmux happily stores tabs in option values; when
// format_expand=true the value goes through tmux's #{format}
// substitution which treats tab as ordinary literal text — so the
// allowance is uniform either way.
//
// The NUL/control rejection guards against argv-poisoning: a stray
// NUL would terminate the Go-side string before tmux saw the rest of
// the value, and a stray control character would make audit / metric
// surfaces hard to read.
func validateSetWindowOptionValue(value string) *rpcError {
	if len(value) > maxSetWindowOptionValueLen {
		return invalidParams("value length %d out of range [0..%d]", len(value), maxSetWindowOptionValueLen)
	}
	if !utf8.ValidString(value) {
		return invalidParams("value: must be valid UTF-8")
	}
	if strings.ContainsRune(value, 0) {
		return invalidParams("value: NUL byte not allowed")
	}
	for i, r := range value {
		// Reject ASCII C0 controls (other than tab) and DEL. These
		// would garble tmux's own diagnostics and never appear in a
		// legitimate option value. \n / \r are unusual but allowed
		// because some format strings legitimately embed them; only
		// the "definitely a mistake" set is rejected.
		if r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return invalidParams("value: control character at byte %d not allowed", i)
		}
	}
	return nil
}

// setWindowOption drives [tmuxctl.Controller.SetWindowOption]. The
// handler does the up-front validation so a caller passing a
// malformed name or value sees CodeInvalidParams (-32602) before any
// tmux command runs.
//
// Validation order:
//   - `name` is required and must match the conservative regex/length
//     policy.
//   - `value` is required when `unset=false`; when `unset=true` it is
//     ignored. Either way the length / control-byte guard runs so a
//     stray field on an unset call still surfaces consistently.
//   - When `global=false` the call needs a target — without it tmux
//     would either pick the "current" window (rarely what an agent
//     meant) or fail with a confusing diagnostic. We require an
//     explicit target there.
//
// Unknown sessions surface via the wrapped errs.ErrSessionNotFound
// sentinel, which the JSON-RPC layer maps to CodeSessionNotFound
// (-32000). Other tmux failures (unknown option name without -q,
// version mismatch) surface as CodeInternal (-32603).
func (t *Tools) setWindowOption(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Name         string `json:"name"`
		Value        string `json:"value"`
		Target       string `json:"target"`
		Append       bool   `json:"append"`
		FormatExpand bool   `json:"format_expand"`
		Global       bool   `json:"global"`
		AllowMissing bool   `json:"allow_missing"`
		Unset        bool   `json:"unset"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("set_window_option: %v", err)
		}
	}
	if rerr := validateSetWindowOptionName(args.Name); rerr != nil {
		return nil, rerr
	}
	// Re-decode into a presence map so we can distinguish "value
	// omitted" from "value is the empty string". When unset=false the
	// field is required; when unset=true it must be absent (tmux
	// would otherwise be passed an extra positional that contradicts
	// the -u flag).
	if !args.Unset {
		if !setWindowOptionFieldPresent(raw, "value") {
			return nil, invalidParams("value: required when unset=false")
		}
		if rerr := validateSetWindowOptionValue(args.Value); rerr != nil {
			return nil, rerr
		}
	}
	// When the caller is not asking for the global table, the call
	// needs a concrete target — without it tmux would pick the
	// "current" window or fail confusingly. The schema marks `target`
	// optional because global=true legitimately omits it.
	if !args.Global {
		if rerr := validateSetWindowOptionTarget(args.Target); rerr != nil {
			return nil, rerr
		}
	} else if args.Target != "" {
		// Even on global=true we still validate a non-empty target so
		// a stray field (e.g. a caller forwarding `target` from a
		// previous session-scope call) cannot smuggle in a malformed
		// value. tmux silently ignores -t when -g is supplied, so this
		// is purely a hygiene guard.
		if rerr := validateSetWindowOptionTarget(args.Target); rerr != nil {
			return nil, rerr
		}
	}
	// scope=window targets `SESSION:WINDOW`; rewrite the leading
	// session segment through the configured -session-prefix when
	// the target carries one. Window-only targets like `@N` have no
	// session segment to rewrite, so the resolver leaves them alone.
	resolvedTarget := t.resolveWindowMoveTarget(args.Target)

	if err := t.Ctl.SetWindowOption(ctx, resolvedTarget, args.Name, args.Value,
		args.Append, args.FormatExpand, args.Global, args.AllowMissing, args.Unset); err != nil {
		return nil, internalError(fmt.Errorf("set_window_option: %w", err))
	}
	return jsonBlock(map[string]any{
		"set":   !args.Unset,
		"unset": args.Unset,
		"name":  args.Name,
	})
}

// setWindowOptionFieldPresent reports whether the JSON-encoded
// arguments carry an explicit `field` key. Used to distinguish
// "field omitted" from "field set to empty string" — tmux happily
// stores empty values for some options (e.g. `history-file ”`), so
// the boundary cannot tell the two cases apart from the Go-side
// zero value alone.
func setWindowOptionFieldPresent(raw json.RawMessage, field string) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m[field]
	return ok
}

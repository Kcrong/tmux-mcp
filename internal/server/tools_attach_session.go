package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// maxAttachSessionFlagsLen caps the length of the optional `flags`
// string forwarded as `-f FLAGS` to tmux. The man page describes flags
// as "a comma-separated list of client flags"; real-world examples top
// out at ~30 bytes (`active-pane,read-only`). 256 leaves comfortable
// headroom for an operator who chains a dozen flags while still
// rejecting a malicious caller stuffing kilobytes into argv.
const maxAttachSessionFlagsLen = 256

// maxAttachSessionWorkingDirLen caps the optional `working_directory`
// string forwarded as `-c WORKING-DIRECTORY` to tmux. Filesystem path
// limits vary across Linux / macOS / BSD; PATH_MAX is 4096 on Linux and
// 1024 on macOS, so 4096 is the cross-platform ceiling. We pin it as a
// constant rather than os-specific so the schema (which advertises
// maxLength) stays portable.
const maxAttachSessionWorkingDirLen = 4096

// attachSessionToolDefs holds the JSON Schema for the attach_session
// tool. It is appended onto the main toolDefs slice via the package
// init() in this file so the registration site stays close to the
// handler — the dispatcher in tools.go only needs the single
// name → handler entry.
//
// The schema marks `target_session` required; every other field is
// optional. The "must opt into a detach flag from a headless server"
// constraint is enforced at the controller boundary (returning
// ErrAttachRequiresTTY which the handler maps to CodeInvalidParams)
// rather than encoded in JSON Schema, because it depends on whether
// the deploying tmux-mcp owns a TTY — knowable only at runtime.
var attachSessionToolDefs = []map[string]any{
	{
		"name": "attach_session",
		"description": "Drive `tmux attach-session [-dDErXx] [-c WORKING-DIRECTORY] " +
			"[-f FLAGS] [-t TARGET-SESSION]` against the named session. " +
			"Important: tmux's attach-session needs a controlling TTY to " +
			"render the session, which the MCP server does NOT have — every " +
			"call arrives over JSON-RPC over stdio. As a result this tool " +
			"implements the meaningful headless interpretation: when " +
			"`detach_others=true` (or `detach_others_including_self=true`) " +
			"is set, the call clears the target session's client roster so a " +
			"follow-up real attach (run from a terminal elsewhere) lands " +
			"cleanly. When neither detach flag is set the call is rejected " +
			"with `-32602` and a message suggesting the caller run " +
			"`tmux attach -t <name>` themselves from a terminal. The " +
			"forward-compat fields (`read_only`, `working_directory`, " +
			"`skip_environment_update`, `flags`, `no_environment_apply`) " +
			"are validated for shape and accepted today — when a future " +
			"build of tmux-mcp grows real TTY support they will land " +
			"verbatim onto the tmux command line. This is a MUTATING tool " +
			"(it changes the server's client roster), so a `-read-only` " +
			"deployment rejects it with `-32011`.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target_session": map[string]any{
					"type":        "string",
					"description": "Required tmux session name to attach to. Same conservative regex/length policy as the rest of the surface (alnum/underscore/dash, 1-64 bytes). Maps to `-t TARGET-SESSION`.",
				},
				"detach_others": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, pass `-d` to detach every other client attached to this session. Combined with `detach_others_including_self`, at least one detach flag MUST be set on a headless server, otherwise the call is refused with `-32602`.",
				},
				"detach_others_including_self": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, pass `-D` (tmux 3.5+) to detach every other client including any attached as us. Older tmux builds emit \"unknown flag -D\"; on tmux-mcp's headless interpretation the controller routes through `detach-client -s SESSION` so the underlying argv works on every supported tmux.",
				},
				"read_only": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, pass `-r` to attach in read-only mode (the client cannot type into the session). Forward-compat: validated today, applied verbatim when tmux-mcp grows real TTY support.",
				},
				"working_directory": map[string]any{
					"type":        "string",
					"maxLength":   maxAttachSessionWorkingDirLen,
					"description": "Optional `-c WORKING-DIRECTORY` value. Must be an absolute path; rejected up front with `-32602` otherwise. Forward-compat: validated today, applied verbatim when tmux-mcp grows real TTY support.",
				},
				"skip_environment_update": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, pass `-E` so tmux skips its `update-environment` pass on attach. Forward-compat: validated today, applied verbatim when tmux-mcp grows real TTY support.",
				},
				"flags": map[string]any{
					"type":        "string",
					"maxLength":   maxAttachSessionFlagsLen,
					"description": "Optional `-f FLAGS` value. Comma-separated list of client flags (see tmux(1) \"CLIENT FLAGS\"); forwarded verbatim, capped at 256 bytes. Forward-compat: validated today, applied verbatim when tmux-mcp grows real TTY support.",
				},
				"no_environment_apply": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, pass `-X` (tmux 3.5+) so tmux does not apply update-environment on attach. Distinct from `skip_environment_update` (`-E`); some builds reject `-X` outright. Forward-compat: validated today, applied verbatim when tmux-mcp grows real TTY support.",
				},
			},
			"required":             []string{"target_session"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register attach_session onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, attachSessionToolDefs...)
}

// validateAttachSessionFlags enforces the up-front guard for the
// optional `flags` argument. Empty is allowed (no `-f` forwarded);
// otherwise the value must satisfy a shape policy that mirrors what
// real tmux client flag lists look like — a comma-separated list of
// alnum / dash / underscore tokens. Whitespace, control chars, and
// shell metacharacters are rejected so a stray quote can never reach
// tmux's argv.
func validateAttachSessionFlags(flags string) *rpcError {
	if flags == "" {
		return nil
	}
	if len(flags) > maxAttachSessionFlagsLen {
		return invalidParams("attach_session: flags length %d out of range [0..%d]", len(flags), maxAttachSessionFlagsLen)
	}
	// Allow letters, digits, comma (the documented separator), dash,
	// and underscore. Anything else is almost certainly a typo or an
	// argv-injection attempt — every documented client flag sticks to
	// this character set ("active-pane", "read-only", "no-output", …).
	for i := 0; i < len(flags); i++ {
		ch := flags[i]
		switch {
		case ch >= 'a' && ch <= 'z':
		case ch >= 'A' && ch <= 'Z':
		case ch >= '0' && ch <= '9':
		case ch == ',' || ch == '-' || ch == '_':
		default:
			return invalidParams("attach_session: flags contains disallowed byte %q at position %d", ch, i)
		}
	}
	return nil
}

// attachSession drives [tmuxctl.Controller.AttachSession]. The handler
// does the up-front validation so a caller passing a malformed
// target_session, an over-sized working_directory, or a stray flag
// shape sees CodeInvalidParams before any tmux command runs.
//
// Validation order:
//   - `target_session` is required and must satisfy the conservative
//     session-name policy (same regex / length rules every other
//     session-bearing tool uses).
//   - `working_directory` is optional; when present it must be an
//     absolute path. The check piggy-backs on validateCwd so a future
//     refactor that tightens the absolute-path rule applies here too.
//   - `flags` is optional; when present it must be a bounded
//     alnum-comma-dash-underscore string. Empty is permitted.
//
// Error mapping:
//   - malformed args / unknown field    → -32602 (CodeInvalidParams).
//   - target session not found          → -32000 (CodeSessionNotFound).
//   - "no detach flag set" (TTY needed) → -32602 with a message
//     suggesting the caller run `tmux attach` from a terminal.
//   - any other tmux failure            → -32603 (CodeInternal).
//
// This is a MUTATING tool — it can detach existing clients — so it is
// deliberately NOT in readOnlyTools; a -read-only deployment rejects
// the call before the handler runs.
func (t *Tools) attachSession(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		TargetSession             string `json:"target_session"`
		DetachOthers              bool   `json:"detach_others"`
		DetachOthersIncludingSelf bool   `json:"detach_others_including_self"`
		ReadOnly                  bool   `json:"read_only"`
		WorkingDirectory          string `json:"working_directory"`
		SkipEnvironmentUpdate     bool   `json:"skip_environment_update"`
		Flags                     string `json:"flags"`
		NoEnvironmentApply        bool   `json:"no_environment_apply"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("attach_session: %v", err)
		}
	}
	// validateSessionRef enforces the conservative
	// alnum/underscore/dash policy (1-64 bytes) so a stray quote /
	// shell metachar never reaches tmux's argv. Same validator
	// every other session-bearing tool uses, which keeps the
	// CodeInvalidParams surface uniform.
	if rerr := validateSessionRef(args.TargetSession); rerr != nil {
		return nil, rerr
	}
	if rerr := validateCwd(args.WorkingDirectory); rerr != nil {
		return nil, rerr
	}
	if rerr := validateAttachSessionFlags(args.Flags); rerr != nil {
		return nil, rerr
	}
	// Resolve through the configured -session-prefix so the tmuxctl
	// call lands on the same prefixed name session_create / has_session
	// / session_kill use. Empty prefix passes the input through
	// verbatim (the back-compat default).
	resolved := t.resolveSessionRef(args.TargetSession)
	err := t.Ctl.AttachSession(ctx, tmuxctl.AttachSessionOpts{
		TargetSession:             resolved,
		DetachOthers:              args.DetachOthers,
		DetachOthersIncludingSelf: args.DetachOthersIncludingSelf,
		ReadOnly:                  args.ReadOnly,
		WorkingDirectory:          args.WorkingDirectory,
		SkipEnvironmentUpdate:     args.SkipEnvironmentUpdate,
		Flags:                     args.Flags,
		NoEnvironmentApply:        args.NoEnvironmentApply,
	})
	if err != nil {
		// "no detach flag set" → the headless server cannot bind a
		// TTY, so the request is structurally impossible to satisfy.
		// Map it onto CodeInvalidParams (the caller's input shape is
		// the problem) with a message that points them at the
		// real-terminal escape hatch.
		if errors.Is(err, tmuxctl.ErrAttachRequiresTTY) {
			return nil, invalidParams(
				"attach_session: cannot attach a TTY from the MCP server context; "+
					"run `tmux attach -t %s` from a terminal, or pass "+
					"detach_others=true / detach_others_including_self=true to "+
					"clear the session's client roster instead",
				args.TargetSession,
			)
		}
		// errs.ErrSessionNotFound (target missing) and any other tmux
		// failure shape get the standard internalError mapping —
		// internalError consults errs.CodeOf so the wrapped sentinel
		// surfaces as -32000 automatically.
		return nil, internalError(fmt.Errorf("attach_session: %w", err))
	}
	// The headless detach succeeded. The wire shape is a flat object
	// keyed by `attached`: a future addition (e.g. "client_count": N
	// or "session": <resolved-name>) lands alongside without breaking
	// callers that read only the boolean. We deliberately echo the
	// caller-supplied logical name, not the resolved (prefixed) one,
	// so a deployment with -session-prefix sees the names it asked
	// for.
	return jsonBlock(map[string]any{
		"attached":       true,
		"target_session": args.TargetSession,
	})
}

package tmuxctl

import (
	"context"
	"errors"
	"fmt"
)

// ErrAttachRequiresTTY is the typed sentinel the controller returns when
// AttachSession is asked to perform a real interactive attach from the
// headless MCP server context. tmux's `attach-session` is a foreground
// terminal-bound operation: it hijacks the calling process's controlling
// TTY to render the target session. The MCP server, by definition, does
// not own a controlling terminal — every command arrives over JSON-RPC
// over stdio — so the only honest response is to refuse the call and
// suggest the caller run `tmux attach -t <name>` themselves from a
// terminal.
//
// The sentinel exists (rather than a free-form error string) so the
// JSON-RPC layer can map it onto CodeInvalidParams uniformly without
// substring-matching the message. Callers that pass `detach_others=true`
// or `detach_others_including_self=true` opt into the meaningful headless
// interpretation — boot every other client off the target session so a
// fresh interactive attach (run elsewhere) lands cleanly — which the
// controller turns into a `detach-client -s SESSION` invocation.
var ErrAttachRequiresTTY = errors.New("attach-session requires a controlling TTY")

// AttachSessionOpts captures the full surface of `tmux attach-session`
// flags exposed via the MCP boundary. The struct lives next to
// AttachSession so both files compile against the same field set without
// a separate header file. Boolean flags map one-for-one onto the tmux
// CLI; string flags are passed through verbatim with the same
// length / control-byte hygiene every other tmuxctl entry point applies.
type AttachSessionOpts struct {
	// TargetSession is the SESSION name passed to `-t TARGET-SESSION`.
	// Required: an empty value is rejected up front rather than letting
	// tmux fall back to "the most recently used session" (a behaviour
	// the headless MCP server has no stable interpretation for).
	TargetSession string
	// DetachOthers maps to `-d`. tmux interprets this as "detach every
	// other client attached to the target session before attaching the
	// new one". Under the headless interpretation we use here (where no
	// real attach happens server-side), it becomes the meaningful
	// signal to clear the session's client roster so a follow-up real
	// attach lands cleanly.
	DetachOthers bool
	// DetachOthersIncludingSelf maps to `-D`. tmux 3.5+ interprets this
	// as "detach every other client attached to the target session,
	// including any that were already attached as us". For tmux versions
	// that don't support `-D` (3.4 and earlier), the operator's tmux
	// will emit "unknown flag -D" which propagates verbatim. Forward-
	// compatible callers can still set this; older deployments should
	// stick to `DetachOthers`.
	DetachOthersIncludingSelf bool
	// ReadOnly maps to `-r`. Asks tmux to attach in read-only mode so
	// the client cannot type into the session. Under the headless
	// interpretation it is informational — there is no real attach to
	// constrain — but we forward the flag so the wire shape stays
	// honest if a future build of tmux-mcp grows true TTY support.
	ReadOnly bool
	// WorkingDirectory maps to `-c WORKING-DIRECTORY`. The cwd hint tmux
	// will use for any new processes spawned through this attach.
	// Validated by the boundary as an absolute path before reaching here.
	WorkingDirectory string
	// SkipEnvironmentUpdate maps to `-E`. When true, tmux skips the
	// `update-environment` pass that copies attach-time environment
	// variables (DISPLAY, SSH_AUTH_SOCK, etc.) into the session. Useful
	// when the operator wants to preserve the session's existing
	// environment unchanged.
	SkipEnvironmentUpdate bool
	// Flags maps to `-f FLAGS`. A comma-separated list of client flags
	// (see tmux(1) "CLIENT FLAGS"). Forwarded verbatim; the boundary
	// applies a length cap so a runaway value cannot poison argv.
	Flags string
	// NoEnvironmentApply maps to `-X`. tmux 3.5+ interprets this as
	// "do not apply the update-environment knob's value when attaching".
	// Distinct from `SkipEnvironmentUpdate` (`-E`): `-X` is the more
	// granular form that preserves a single env var while still
	// running the rest of the update pass. tmux 3.4 and earlier emit
	// "unknown flag -X"; the error propagates verbatim.
	NoEnvironmentApply bool
}

// AttachSession wraps `tmux attach-session [-dDErXx] [-c WORKING-DIRECTORY]
// [-f FLAGS] [-t TARGET-SESSION]`. The MCP server cannot actually attach
// a TTY — every call arrives over JSON-RPC over stdio — so the controller
// implements the meaningful headless interpretation:
//
//   - When at least one of `DetachOthers` / `DetachOthersIncludingSelf`
//     is set: pre-flight a `has-session -t TARGET` to confirm the
//     session exists, then run `detach-client -s TARGET` to clear its
//     client roster. The natural use-case is "boot every other client
//     off this session so a fresh interactive attach (run elsewhere)
//     lands cleanly" — which the headless server CAN do.
//   - When neither detach flag is set: the operation reduces to "attach
//     this terminal to that session", which the MCP server has no
//     terminal to perform. Returns ErrAttachRequiresTTY so the JSON-RPC
//     layer can map it to CodeInvalidParams with a message suggesting
//     the caller run `tmux attach -t <name>` themselves.
//
// Idempotent semantics: the headless contract means a second call with
// detach flags set is a successful no-op (zero remaining clients to
// detach, tmux's "no current client" path already maps to nil inside
// DetachClient).
//
// Error mapping:
//   - empty TargetSession        → free-form error (boundary rejects this earlier).
//   - target session not found   → wrapped errs.ErrSessionNotFound via has-session.
//   - neither detach flag set    → ErrAttachRequiresTTY (CodeInvalidParams).
//   - detach-client failure      → propagated verbatim (CodeInternal).
//
// The flag-bearing fields (ReadOnly, WorkingDirectory, SkipEnvironmentUpdate,
// Flags, NoEnvironmentApply) are accepted for forward compatibility: when
// a future tmux-mcp grows real TTY support those fields will land verbatim
// onto the tmux command line. Today they are validated for shape but
// otherwise ignored on the headless detach path.
func (c *Controller) AttachSession(ctx context.Context, opts AttachSessionOpts) error {
	if opts.TargetSession == "" {
		return errors.New("attach-session: target_session required")
	}
	// The headless server cannot bind a controlling TTY, so we refuse
	// the no-detach shape up front. Callers that genuinely want to
	// reset the session's client roster opt into one of the detach
	// flags; everyone else gets a clean error directing them to run
	// `tmux attach -t <name>` from a real terminal.
	if !opts.DetachOthers && !opts.DetachOthersIncludingSelf {
		return fmt.Errorf("attach-session %s: %w", opts.TargetSession, ErrAttachRequiresTTY)
	}
	// Pre-flight: confirm the session exists. has-session emits
	// "can't find session: <name>" with rc=1 when the target is
	// missing, which run() already maps to errs.ErrSessionNotFound via
	// isSessionMissingMsg. Doing the explicit probe here (rather than
	// letting detach-client implicitly handle it) keeps the typed
	// sentinel reliable even though detach-client's own missing-session
	// branch is intentionally idempotent.
	if _, err := c.run(ctx, "has-session", "-t", opts.TargetSession); err != nil {
		// Already wrapped with errs.ErrSessionNotFound by run() when the
		// session is missing. Pass through any other failure verbatim.
		return err
	}
	// Headless interpretation of `-d` / `-D`: detach every client
	// currently on the target session so a follow-up real attach
	// (issued from a terminal elsewhere) lands cleanly. We funnel
	// through DetachClient so the existing "no current client → nil"
	// idempotency contract handles the empty-roster case for us.
	if err := c.DetachClient(ctx, "", opts.TargetSession, false); err != nil {
		return fmt.Errorf("attach-session %s: %w", opts.TargetSession, err)
	}
	return nil
}

// IsAttachRequiresTTYErr reports whether err originated from
// AttachSession's "no detach flag set, no TTY available" refusal. The
// JSON-RPC layer uses this to map the failure onto CodeInvalidParams
// rather than the default CodeInternal — the underlying issue is a
// caller misconception about what the MCP server can do, not a tmux
// failure.
//
// Implemented as a thin errors.Is wrapper so test fixtures that
// construct synthetic errors can target the same sentinel without
// importing the package's internals.
func IsAttachRequiresTTYErr(err error) bool {
	return errors.Is(err, ErrAttachRequiresTTY)
}

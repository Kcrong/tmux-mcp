package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// displayPanesNoCurrentClientMatchers covers the various phrasings
// tmux can emit when `display-panes` runs against a server that has no
// attached clients (which is the common case for the headless tmux
// servers tmux-mcp owns). A bare `display-panes` with no `-t` resolves
// against the caller's "current" client; if there is none, tmux returns
// non-zero with one of these messages. Treating them as a successful
// no-op keeps fire-and-forget callers from having to substring-match
// stderr — a future contributor that adds another "nothing to show"
// phrasing should extend the match here.
//
// Detection is by message text rather than exit-code shape because
// every display-panes failure exits non-zero; the message is the only
// signal that pins down which failure mode tmux hit.
var displayPanesNoCurrentClientMatchers = []string{
	"no current client",
	"no current target",
}

// isDisplayPanesNoClientMsg reports whether stderr text from
// `tmux display-panes` indicates "there is no client to draw the picker
// on". Any match folds the error onto a successful no-op so an agent
// firing `display_panes` against a headless server doesn't have to
// pre-flight `list_clients`.
func isDisplayPanesNoClientMsg(msg string) bool {
	low := strings.ToLower(msg)
	for _, m := range displayPanesNoCurrentClientMatchers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// isDisplayPanesClientMissingMsg recognises tmux's "can't find client:
// <name>" stderr when a `-t <client>` target does not match any
// attached client. We map this onto errs.ErrSessionNotFound so the
// JSON-RPC dispatcher can route it to CodeSessionNotFound — the typed
// sentinel the rest of the surface (list_clients, session_kill,
// detach_client, …) reuses for "named target does not exist". Sharing
// the code keeps clients from having to learn a per-tool failure
// vocabulary.
//
// "can't find session" is already recognised by run()'s
// isSessionMissingMsg, so the wrapping happens automatically inside
// run(); we only need to handle the client-name variant explicitly.
func isDisplayPanesClientMissingMsg(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "can't find client")
}

// DisplayPanesOpts bundles the optional flags the agent can steer on a
// single display_panes call. Every field is optional; the controller
// builds the actual `tmux display-panes` argv from whichever fields are
// set. Sticking to a struct (rather than positional args) keeps the
// controller boundary stable as new tmux flags land — adding `-N` later
// would otherwise force every existing caller to rewrite its call.
type DisplayPanesOpts struct {
	// Block, when true, pins `-b` onto the argv: tmux waits until the
	// user has finished selecting a pane (or pressed Escape) before
	// returning. Without `-b` the call returns as soon as the picker is
	// drawn, which is what the default fire-and-forget contract wants.
	Block bool
	// Duration, when non-zero, pins `-d <ms>` onto the argv: tmux paints
	// the picker for the given number of milliseconds before clearing
	// it. Zero leaves the flag off, in which case tmux's own default
	// (display-panes-time, typically 1000ms) applies. Anything outside
	// [0..maxDisplayPanesDurationMs] is rejected at the boundary so a
	// hostile caller can't pin a tmux client into the picker for
	// minutes.
	Duration time.Duration
	// NoPrefix, when true, pins `-N` onto the argv: tmux's normal
	// behaviour reserves the prefix key during the picker, but `-N`
	// frees it so the user can drop straight into a normal binding
	// without first dismissing the picker.
	NoPrefix bool
	// Target, when non-empty, pins `-t <client>` onto the argv: tmux
	// draws the picker on the named client (a TTY path like
	// "/dev/pts/3"). Empty draws on the caller's current client, which
	// the headless servers tmux-mcp owns rarely have.
	Target string
	// Template, when non-empty, becomes the trailing positional arg
	// tmux runs against the user's selection (e.g. "select-pane -t %%"
	// to focus the picked pane). Empty leaves the trailing slot empty,
	// in which case tmux's default template (also "select-pane -t %%"
	// in practice) applies.
	Template string
}

// maxDisplayPanesDurationMs caps the upper bound the controller will
// accept on Duration. tmux happily accepts huge values but pinning the
// picker for longer than 10 minutes makes no sense for an agent and
// would let a hostile caller hold a live client in a useless state for
// long stretches. Mirrors the JSON-RPC layer's maxDurationMs so a
// future rebase that consolidates the validators sees no drift.
const maxDisplayPanesDurationMs = 600000

// DisplayPanes drives `tmux display-panes [-b] [-d duration] [-N] [-t client] [template]`.
// It is the controller-side entry point for the display_panes MCP tool.
// The flag mapping mirrors the DisplayPanesOpts comments verbatim; see
// there for the per-flag rationale.
//
// Caller contract:
//
//   - opts.Duration is converted to whole milliseconds via Round before
//     being passed on. Negative values are rejected up front; values
//     above maxDisplayPanesDurationMs are rejected for the same reason
//     the JSON-RPC layer caps every *_ms knob — a runaway caller must
//     not be able to pin a client in the picker for an unbounded
//     duration. The conversion uses Round (not Truncate) so a 999.4ms
//     opt rounds to 999, and a 999.6ms opt rounds to 1000 — close
//     enough to user intent without invented precision.
//   - opts.Target, when non-empty, must be the TTY-path shape of a
//     real attached client. The controller does not pre-validate the
//     shape (the JSON-RPC layer does that work); a target that does
//     not match an attached client surfaces here as a wrapped
//     errs.ErrSessionNotFound so the dispatcher can map onto
//     CodeSessionNotFound uniformly with the rest of the surface.
//   - opts.Template is forwarded verbatim. tmux substitutes `%%` (the
//     selected pane id) and a small number of other format tokens; we
//     do NOT try to validate those at the controller layer because
//     tmux's format grammar is large and version-dependent. The
//     JSON-RPC layer caps the length up front so a hostile template
//     cannot inflate the argv.
//
// Headless-server fold:
//
//	On a server with nothing attached, tmux exits non-zero with "no
//	current client" stderr (or "no current target"; both are folded
//	via isDisplayPanesNoClientMsg). The controller maps that onto nil
//	so a fire-and-forget display_panes against an empty roster looks
//	like a clean success rather than an error the caller must
//	substring-match. Mirrors the same fold detach_client uses.
//
// Missing-client mapping:
//
//	When `-t <client>` names a client that does not exist, tmux emits
//	"can't find client: <name>"; we wrap that into errs.ErrSessionNotFound
//	so the dispatcher's existing CodeSessionNotFound path catches it
//	without a per-tool branch.
func (c *Controller) DisplayPanes(ctx context.Context, opts DisplayPanesOpts) error {
	if opts.Duration < 0 {
		return fmt.Errorf("display-panes: duration must be non-negative, got %s", opts.Duration)
	}
	durMs := int(opts.Duration.Round(time.Millisecond) / time.Millisecond)
	if durMs > maxDisplayPanesDurationMs {
		return fmt.Errorf("display-panes: duration %s exceeds %dms cap",
			opts.Duration, maxDisplayPanesDurationMs)
	}
	args := []string{"display-panes"}
	if opts.Block {
		args = append(args, "-b")
	}
	if durMs > 0 {
		// tmux's `-d` accepts an integer count of milliseconds (the
		// same units display-panes-time itself takes). Zero means
		// "leave the flag off and let tmux's own default apply".
		args = append(args, "-d", strconv.Itoa(durMs))
	}
	if opts.NoPrefix {
		args = append(args, "-N")
	}
	if opts.Target != "" {
		args = append(args, "-t", opts.Target)
	}
	if opts.Template != "" {
		// Template is the trailing positional arg; tmux interprets the
		// rest of argv as flags up to the first non-flag token, so a
		// template that starts with "-" is still safely demarcated by
		// the preceding flags above. We deliberately do NOT prefix
		// with "--" because tmux's argument parser treats "--" as a
		// literal token in some versions (forwarded into the user's
		// command), which would silently break the caller's template.
		args = append(args, opts.Template)
	}
	if _, err := c.run(ctx, args...); err != nil {
		// Headless server / nothing attached: fold onto nil so a
		// fire-and-forget display_panes against an empty roster looks
		// like a clean success.
		if isDisplayPanesNoClientMsg(err.Error()) {
			return nil
		}
		// Named-but-missing client target: wrap into ErrSessionNotFound
		// so the dispatcher's existing CodeSessionNotFound path catches
		// it. Guard against double-wrapping in case run() ever starts
		// recognising the client-name variant itself.
		if !errors.Is(err, errs.ErrSessionNotFound) && isDisplayPanesClientMissingMsg(err.Error()) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

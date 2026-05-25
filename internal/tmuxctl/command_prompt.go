package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// CommandPrompt drives `tmux command-prompt [-1iIN] [-p PROMPTS] [-I INPUTS]
// [-t TARGET] [TEMPLATE]`, the tmux command that asks the targeted client
// to open its interactive command-prompt UI. The agent uses this to
// programmatically launch preset prompt dialogs (e.g. a rename-window
// flow whose template is `rename-window %%`); when the user fills in the
// prompt(s) tmux executes TEMPLATE with each `%%` placeholder substituted
// by the corresponding input. The four boolean knobs map directly onto
// tmux's flags: oneKey (-1) for single-keypress accept, incremental (-i)
// for "run on every keystroke", multiLine (-N) for the rare multi-line
// editor mode, and the implicit "must have a client" rule below.
//
// Headless behaviour: tmux's command-prompt is meaningless without an
// attached client (the prompt is rendered into the client's status
// line). When this controller's tmux server has no current client and
// the caller did not pin one with `client`, tmux fails with "no current
// client" — we recognise that exact phrase and return nil. This mirrors
// the headless-client idiom every other client-targeting tmuxctl method
// applies (see refresh_client / display_message / list_clients): a
// no-op on a headless server is the right answer because a successful
// dispatch would have rendered into a client that isn't there.
//
// When the caller passes an explicit `client` (a TTY path) and tmux
// reports "can't find client", we wrap errs.ErrSessionNotFound so the
// JSON-RPC layer can map it to CodeSessionNotFound — the closest stable
// code we expose for "the addressed thing does not exist". We do this
// only for the explicit-client branch because the no-client branch
// already returned nil above; surfacing the not-found there would
// conflict with the headless no-op contract.
//
// The four string args (client, prompts, inputs, template) are
// forwarded to tmux verbatim — every length / encoding rule is enforced
// at the boundary layer (the server tool) before this method is
// reached. We still apply a defensive "no NUL byte" check here because
// tmux silently truncates argv at the first NUL, and a programmatic
// caller bypassing the boundary should not be able to smuggle a
// truncating byte through. The boolean knobs are always safe by
// construction (Go bools).
func (c *Controller) CommandPrompt(
	ctx context.Context,
	client, prompts, inputs, template string,
	oneKey, incremental, multiLine bool,
) error {
	// Defensive NUL check: argv-truncating bytes must not reach tmux,
	// even though the boundary layer rejects them up front. Empty
	// strings short-circuit so the caller's "omit this arg" semantics
	// still work — tmux only sees the corresponding flag when the
	// matching field is non-empty.
	for name, val := range map[string]string{
		"client":   client,
		"prompts":  prompts,
		"inputs":   inputs,
		"template": template,
	} {
		if strings.ContainsRune(val, '\x00') {
			return fmt.Errorf("%s: must not contain NUL", name)
		}
	}

	args := []string{"command-prompt"}
	if oneKey {
		// -1 makes the prompt accept a single keypress (no Enter
		// required). Pairs naturally with template "select-window -t :%%"
		// style flows where the user picks a number.
		args = append(args, "-1")
	}
	if incremental {
		// -i runs the command on every keystroke (incremental search).
		// Useful for live-filter UX, but rarely what a programmatic
		// caller wants — the caller decides.
		args = append(args, "-i")
	}
	if multiLine {
		// -N opens a multi-line editor instead of the single-line
		// prompt. Rarely used; exposed for completeness so the caller
		// has the full flag surface.
		args = append(args, "-N")
	}
	if prompts != "" {
		// -p PROMPTS is a comma-separated list of prompt strings;
		// tmux uses one prompt per `%%` placeholder in TEMPLATE.
		args = append(args, "-p", prompts)
	}
	if inputs != "" {
		// -I INPUTS is the matching comma-separated list of *default*
		// inputs (preset values shown in each prompt slot). Empty
		// segments mean "no default for this slot".
		args = append(args, "-I", inputs)
	}
	if client != "" {
		// -t pins which client renders the prompt UI. Without it tmux
		// uses its current client; on a headless server (the common
		// tmux-mcp case) that means "no client" and the call short-
		// circuits to nil below.
		args = append(args, "-t", client)
	}
	if template != "" {
		// Final positional arg: the tmux command tmux runs once the
		// prompt(s) are filled. `%%` inside it is replaced by the user
		// input on submission. tmux accepts an empty/missing template
		// (it just opens the prompt with no execute step), so we
		// mirror that.
		args = append(args, template)
	}

	if _, err := c.run(ctx, args...); err != nil {
		// Headless idiom: a tmux-mcp server typically has no attached
		// client, so a `command-prompt` without `-t` lands on "no
		// current client". Treat that as a successful no-op so callers
		// don't have to special-case the headless surface every time.
		// The matcher is intentionally narrow (substring rather than
		// regex) so a future tmux build whose phrasing differs slightly
		// still hits this branch.
		if isCmdPromptNoClientMsg(err.Error()) {
			return nil
		}
		// Explicit-client branch: when the operator pinned a TTY and
		// tmux can't find that client, surface a typed
		// errs.ErrSessionNotFound so the JSON-RPC layer maps it to
		// CodeSessionNotFound. This is the closest existing code for
		// "the addressed entity is not there", reused here for client
		// targets the same way display_message / list_clients reuse it
		// for missing windows / panes.
		if client != "" && !errors.Is(err, errs.ErrSessionNotFound) {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "can't find client") {
				return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
			}
		}
		return err
	}
	return nil
}

// isCmdPromptNoClientMsg reports whether stderr from `tmux command-prompt`
// indicates the call had no client to render against. tmux's exact
// phrasing has been "no current client" across every 3.x build we
// support; the substring match keeps the test safe against a hypothetical
// case-tweak ("No current client" / trailing punctuation) without
// pulling in a regex dependency. Naming this helper for the command-
// prompt site rather than something generic avoids colliding with
// matchers other (refresh-client, display-message) tools may grow.
func isCmdPromptNoClientMsg(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "no current client")
}

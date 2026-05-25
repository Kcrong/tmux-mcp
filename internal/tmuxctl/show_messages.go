package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// ShowMessages wraps `tmux show-messages [-JT] [-t CLIENT]` and returns
// the per-client message log tmux maintains for the bottom status bar.
// One element per emitted line, with no trailing '\n' on each entry; an
// empty slice means tmux had nothing to show.
//
// Flag mapping:
//
//   - client != ""        → `-t CLIENT`
//   - includeJobs=true    → `-J` (append the job log to the output)
//   - includeTerminal=true → `-T` (append the terminal log to the output)
//
// Headless contract: the tmux servers tmux-mcp owns rarely have a
// client attached, and show-messages without a target reports
// "no current client" with rc=1 in that case. That is the load-bearing
// idempotent path for an introspection tool — return an empty slice
// (no error) so an agent can call this at any point without first
// having to attach a client. The same is true when the daemon has not
// been spun up yet ("no server running", "error connecting"): zero
// messages exist by definition, so the empty-slice contract holds.
//
// When client is non-empty and tmux can't find that client we wrap
// errs.ErrSessionNotFound so the JSON-RPC dispatcher maps it to
// CodeSessionNotFound (-32000) — symmetric with every other targeted
// inspection tool. Other tmux failures pass through unchanged so the
// dispatcher surfaces them via CodeInternal.
func (c *Controller) ShowMessages(ctx context.Context, client string, includeJobs, includeTerminal bool) ([]string, error) {
	args := []string{"show-messages"}
	switch {
	case includeJobs && includeTerminal:
		// tmux accepts the combined `-JT` form; forwarding it as a single
		// token mirrors the way an operator would type the command on
		// the CLI and keeps the argv shape stable across tmux versions.
		args = append(args, "-JT")
	case includeJobs:
		args = append(args, "-J")
	case includeTerminal:
		args = append(args, "-T")
	}
	if client != "" {
		args = append(args, "-t", client)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		msg := strings.ToLower(err.Error())
		// "no current client" is tmux's response when show-messages
		// is invoked without an attached client. For the headless
		// servers tmux-mcp owns, that is the common case — surface it
		// as a clean empty slice so an agent can introspect at any
		// time without first having to attach. Same reasoning applies
		// to "no server running" / "error connecting": a daemon that
		// hasn't started yet has, by definition, zero messages.
		if client == "" && (strings.Contains(msg, "no current client") ||
			strings.Contains(msg, "no server running") ||
			strings.Contains(msg, "error connecting")) {
			return nil, nil
		}
		// `tmux show-messages -t <client>` rejects an unknown client
		// with "can't find client: <name>"; translate it into the
		// typed sentinel so the JSON-RPC dispatcher can map every
		// "target not found" surface uniformly.
		if client != "" && !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(msg, "can't find client") {
			return nil, fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return nil, err
	}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		// No messages logged yet — common on a fresh server. Return
		// an empty slice (nil) so callers can iterate without a
		// separate "is this an error" branch.
		return nil, nil
	}
	// tmux emits one message per line; split verbatim so blank
	// in-between lines (rare, but possible when an entry was empty)
	// are preserved as-is. Each element drops the trailing '\n' that
	// the controller already trimmed off the buffer above.
	return strings.Split(out, "\n"), nil
}

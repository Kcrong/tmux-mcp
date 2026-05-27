package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// ChooseBuffer wraps `tmux choose-buffer [-N] [-Z] [-r] [-t TARGET-PANE]
// [-F FORMAT] [-f FILTER] [-K KEY-FORMAT] [-O SORT-ORDER] [TEMPLATE]`.
//
// Surface intent: tmux's `choose-buffer` opens an interactive
// buffer-chooser inside the target pane. Each tmux-mcp call is a
// "fire-and-forget" entrance into that mode — the controller does not
// drive the picker, it merely puts the pane into buffer-mode so a
// follow-up `send_keys` (or a real client attached to the server) can
// step through the buffer list. Use this from an LLM agent that wants
// to expose tmux's native paste-buffer browser without re-implementing
// the navigation UI on the JSON-RPC side.
//
// The argv builder forwards exactly the flags the caller set:
//   - `-N` (no preview),
//   - `-Z` (zoom the chooser pane),
//   - `-r` (reverse the listing),
//   - `-t TARGET-PANE` (the pane that should enter buffer-mode),
//   - `-F FORMAT` (template the chooser displays for each row),
//   - `-f FILTER` (Boolean format pruning the row set),
//   - `-K KEY-FORMAT` (per-row key-shortcut template),
//   - `-O SORT-ORDER` (one of `time`, `name`, `size`, ...),
//   - the trailing TEMPLATE is the command tmux runs on the
//     selected buffer (e.g. `paste-buffer -b %%`).
//
// Empty values short-circuit the corresponding flag so callers can
// forward optional CLI args without an extra branch — passing
// `format=""` is byte-equivalent to omitting `-F` entirely. This
// matches the rest of the controller surface (Capture, ShowOptions,
// DisplayMessage, ...).
//
// Error contract:
//   - A missing target pane (or "no current target" when no `-t` was
//     supplied and tmux can't resolve a current pane) surfaces as a
//     wrapped errs.ErrSessionNotFound. Callers can errors.Is into the
//     sentinel regardless of which exact tmux phrase landed on stderr
//     ("can't find pane", "no current target", "no client").
//   - A "no server running" / "error connecting" stderr is treated as
//     the same headless case — there is no pane to enter buffer-mode
//     on — so the JSON-RPC layer maps it to CodeSessionNotFound just
//     like the other "missing target" surfaces.
//   - Other tmux failures pass through unchanged so the dispatcher
//     surfaces them via CodeInternal.
//
// The boundary (server tool) is responsible for the up-front
// regex/length checks on every string argument; the controller passes
// the validated values verbatim to tmux so a stray quote or
// argv-injection attempt cannot slip through.
func (c *Controller) ChooseBuffer(
	ctx context.Context,
	target, format, filter, keyFormat, sortOrder, template string,
	noPreview, zoom, reverse bool,
) error {
	args := []string{"choose-buffer"}
	// Boolean flags mirror tmux's argv shape: `-N`, `-Z`, `-r` are
	// independent toggles. We forward only the ones the caller set so
	// the controller never silently fakes a default the user didn't
	// ask for.
	if noPreview {
		args = append(args, "-N")
	}
	if zoom {
		args = append(args, "-Z")
	}
	if reverse {
		args = append(args, "-r")
	}
	if target != "" {
		args = append(args, "-t", target)
	}
	if format != "" {
		args = append(args, "-F", format)
	}
	if filter != "" {
		args = append(args, "-f", filter)
	}
	if keyFormat != "" {
		args = append(args, "-K", keyFormat)
	}
	if sortOrder != "" {
		args = append(args, "-O", sortOrder)
	}
	if template != "" {
		// TEMPLATE is positional in tmux's argv — it follows every flag
		// and is the command run on whichever buffer the user selects.
		args = append(args, template)
	}
	if _, err := c.run(ctx, args...); err != nil {
		// `tmux choose-buffer -t <missing>` emits "can't find pane:" or
		// "no current target" depending on whether `-t` was supplied;
		// "no server running" / "error connecting" surface when the
		// daemon isn't up yet. All of them mean the same thing at this
		// layer — we have nowhere to drop the chooser into — so they
		// uniformly wrap errs.ErrSessionNotFound and the JSON-RPC layer
		// maps every "missing surface" case to CodeSessionNotFound.
		if !errors.Is(err, errs.ErrSessionNotFound) {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "can't find pane") ||
				strings.Contains(msg, "can't find window") ||
				strings.Contains(msg, "no current target") ||
				strings.Contains(msg, "no current client") ||
				strings.Contains(msg, "no current session") ||
				strings.Contains(msg, "no server running") ||
				strings.Contains(msg, "error connecting") {
				return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
			}
		}
		return err
	}
	return nil
}

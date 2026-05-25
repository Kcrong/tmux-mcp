package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// ChooseClient wraps `tmux choose-client [-N] [-Z] [-r] [-t TARGET-PANE]
// [-F FORMAT] [-f FILTER] [-K KEY-FORMAT] [-O SORT-ORDER] [TEMPLATE]`.
//
// `tmux choose-client` opens an interactive client-chooser inside the
// target pane: the attached client sees a list of every client currently
// connected to the tmux server and may pick one. The mapping from
// arguments to flags is one-for-one with the man-page surface; missing
// arguments simply omit the corresponding flag so tmux picks its
// documented default for each (e.g. format/filter/sort-order all fall
// back to the built-in client-chooser layout).
//
// Boolean flags (noPreview/zoom/reverse) gate `-N` (suppress preview),
// `-Z` (zoom the chooser pane), and `-r` (reverse the sort order)
// respectively. The remaining string arguments are forwarded verbatim
// when non-empty: the boundary (server tool) is responsible for the
// up-front regex/length checks against pane targets and format strings;
// this method passes the value through to tmux unchanged.
//
// Surface intent: the chooser is a UX affordance for whichever client is
// currently driving the session — it cannot do anything useful when no
// client is attached and tmux has no pane to draw the menu in. Two
// failure modes therefore surface as wrapped errs.ErrSessionNotFound so
// the JSON-RPC dispatcher maps them uniformly to CodeSessionNotFound:
//
//   - Missing target pane: when `target` is non-empty, an explicit
//     `display-message -t <target>` probe is run first so the caller sees
//     the typed sentinel instead of the silent no-op `tmux choose-client
//     -t <unknown>` would otherwise return on some tmux versions.
//   - Headless server: when the controller's tmux server has no clients
//     attached (the common case for the headless servers tmux-mcp owns),
//     choose-client would be queued without a visible recipient and never
//     execute. We refuse the call up front so an agent does not "fire and
//     forget" a chooser nobody can see; the typed sentinel makes the
//     refusal recognisable via errors.Is.
//
// Other tmux failures pass through unchanged so the dispatcher surfaces
// them via CodeInternal.
func (c *Controller) ChooseClient(
	ctx context.Context,
	target, format, filter, keyFormat, sortOrder, template string,
	noPreview, zoom, reverse bool,
) error {
	// Probe the target pane up front when one was supplied so the caller
	// sees errs.ErrSessionNotFound on a stale reference. We use
	// `display-message -t <target>` because it returns a non-zero exit
	// with a recognisable "can't find pane" / "can't find session"
	// stderr the run() helper already maps to the typed sentinel —
	// `choose-client -t <unknown>` itself is not a reliable probe across
	// tmux versions (some queue the chooser silently and exit 0).
	if target != "" {
		if _, err := c.run(ctx, "display-message", "-p", "-t", target, "ok"); err != nil {
			if !errors.Is(err, errs.ErrSessionNotFound) {
				msg := strings.ToLower(err.Error())
				if strings.Contains(msg, "can't find pane") ||
					strings.Contains(msg, "can't find window") {
					return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
				}
			}
			return err
		}
	}
	// Refuse to fire choose-client on a headless server. tmux would
	// silently queue the chooser without a visible recipient — the
	// typed sentinel here lets the JSON-RPC layer map the refusal to
	// CodeSessionNotFound so an agent gets a structured signal instead
	// of an apparent success.
	clients, err := c.ListClients(ctx, "")
	if err != nil {
		return err
	}
	if len(clients) == 0 {
		return fmt.Errorf("choose-client: no clients attached: %w", errs.ErrSessionNotFound)
	}
	// argv builder forwards only the set flags, mirroring the tmux
	// man-page ordering so the resulting command is easy to compare
	// against `tmux choose-client -h` output.
	args := []string{"choose-client"}
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
		args = append(args, template)
	}
	if _, err := c.run(ctx, args...); err != nil {
		// Some tmux builds prefer "can't find pane" stderr for an
		// invalid -t even when the upstream display-message probe
		// happened to succeed (e.g. the pane went away between the two
		// calls). Translate that into the same typed sentinel so the
		// JSON-RPC dispatcher maps every "session/pane not found"
		// surface uniformly.
		if !errors.Is(err, errs.ErrSessionNotFound) {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "can't find pane") ||
				strings.Contains(msg, "can't find window") {
				return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
			}
		}
		return err
	}
	return nil
}

package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// ResizePane wraps `tmux resize-pane -t TARGET -{U|D|L|R} AMOUNT`.
// `direction` selects the side the pane grows toward — "up" / "down" /
// "left" / "right" map to tmux's -U / -D / -L / -R respectively, and
// `amount` is the cell count tmux moves the boundary by. The boundary
// (server tool) is responsible for the up-front regex/length check on
// target, the direction whitelist, and the [1..200] bound on amount;
// the controller still refuses obviously-broken inputs so a stray
// internal caller cannot reach tmux with an empty target or zero step.
//
// A missing session/pane surfaces as a wrapped errs.ErrSessionNotFound
// so the JSON-RPC dispatcher maps it to CodeSessionNotFound — the same
// contract every other pane-scoped tmuxctl method upholds. tmux phrases
// the missing-target case as "can't find pane" rather than the
// "session not found" run() already maps; translate that explicitly so
// callers can errors.Is into the typed sentinel regardless of which
// variant tmux emitted.
func (c *Controller) ResizePane(ctx context.Context, target, direction string, amount int) error {
	if target == "" {
		return errors.New("target required")
	}
	flag, err := resizeDirectionFlag(direction)
	if err != nil {
		return err
	}
	if amount <= 0 {
		return fmt.Errorf("amount %d must be positive", amount)
	}
	if _, err := c.run(ctx, "resize-pane", "-t", target, flag, strconv.Itoa(amount)); err != nil {
		// tmux resize-pane against a missing pane says "can't find pane"
		// rather than the "session not found" form run() already maps.
		// Translate it so callers can errors.Is into errs.ErrSessionNotFound
		// regardless of which exact phrase tmux emitted.
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find pane") {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

// resizeDirectionFlag maps the public direction string onto the tmux
// flag that selects the resize axis. The check is exhaustive so a
// future "horizontal" / "vertical" alias can't slip through silently —
// the controller refuses unknown values up front, keeping symmetry
// with the boundary's invalidParams guard.
func resizeDirectionFlag(direction string) (string, error) {
	switch direction {
	case "up":
		return "-U", nil
	case "down":
		return "-D", nil
	case "left":
		return "-L", nil
	case "right":
		return "-R", nil
	default:
		return "", fmt.Errorf("direction %q must be \"up\", \"down\", \"left\", or \"right\"", direction)
	}
}

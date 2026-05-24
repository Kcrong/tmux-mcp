package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// BreakPane wraps `tmux break-pane -P -F "#{window_id}" -s TARGET`. tmux
// detaches the targeted pane from its window and re-homes it as the
// solitary pane of a freshly-created window. The window id (`#{window_id}`,
// e.g. "@7") is stable for the lifetime of that new window and unique
// across the whole tmux server, so callers can hand it straight to
// follow-up window_select / list_panes calls without re-listing the
// session.
//
// target uses tmux's standard pane-target form (e.g. "demo:0.1",
// "demo:0", "demo") — the boundary (server tool) is responsible for the
// up-front regex/length check; the controller passes the value verbatim
// to tmux. -P prints the new window's identity, -F pins the format so
// we don't depend on tmux's default (which has changed across versions).
//
// A missing session/pane surfaces as a wrapped errs.ErrSessionNotFound
// so the JSON-RPC dispatcher maps it to CodeSessionNotFound — the same
// contract every other tmuxctl pane-scoped method upholds. tmux phrases
// the missing-target case as "can't find pane" rather than the
// "session not found" form run() already maps; translate that
// explicitly so callers can errors.Is into the typed sentinel
// regardless of which exact message tmux emitted.
func (c *Controller) BreakPane(ctx context.Context, target string) (string, error) {
	if target == "" {
		return "", errors.New("target required")
	}
	out, err := c.run(ctx, "break-pane", "-P", "-F", "#{window_id}", "-s", target)
	if err != nil {
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find pane") {
			return "", fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return "", err
	}
	window := strings.TrimRight(out, "\n")
	if window == "" {
		return "", fmt.Errorf("break-pane: empty window id from %q", out)
	}
	return window, nil
}

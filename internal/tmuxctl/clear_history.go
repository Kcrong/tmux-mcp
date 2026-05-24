package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// ClearHistory wraps `tmux clear-history -t TARGET`. target uses tmux's
// standard pane-target form (e.g. "demo:0.1", "demo:0", "demo") — the
// boundary (server tool) is responsible for the up-front regex/length
// check; the controller passes the value verbatim to tmux.
//
// Use this when a long-running interactive command (build watcher, log
// tail) has accumulated megabytes of scrollback that bloats `capture`
// payloads and snapshot diffs. clear-history only drops the scrollback
// buffer — the visible region is left untouched, the running process is
// undisturbed, and the pane id stays valid across the call.
//
// A missing session/pane surfaces as a wrapped errs.ErrSessionNotFound
// so the JSON-RPC dispatcher maps it to CodeSessionNotFound — same
// contract every other tmuxctl method upholds. tmux phrases the
// missing-target case as "can't find pane:" or "no current target"
// depending on the form of the target string (and the version on PATH);
// translate both into the same typed sentinel run() emits for "session
// not found" so callers can errors.Is into errs.ErrSessionNotFound
// regardless of which variant tmux happened to emit.
func (c *Controller) ClearHistory(ctx context.Context, target string) error {
	if target == "" {
		return errors.New("target required")
	}
	if _, err := c.run(ctx, "clear-history", "-t", target); err != nil {
		msg := strings.ToLower(err.Error())
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			(strings.Contains(msg, "can't find pane") ||
				strings.Contains(msg, "no current target")) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// SourceFile wraps `tmux source-file [-q] PATH`. path is the absolute
// filesystem path to a tmux config file the controller's tmux server
// should re-source — useful for hot-reloading tweaks (status bar,
// key bindings, options) without restarting the server. quiet maps to
// `-q`, which tells tmux to suppress non-fatal errors (e.g. unknown
// options) so a partially-incompatible config still reloads as far as
// it can. The boundary (server tool) is responsible for the up-front
// shape / safety checks; the controller passes the value verbatim to
// tmux.
//
// A missing file surfaces as a wrapped errs.ErrSessionNotFound so the
// JSON-RPC dispatcher maps it to CodeSessionNotFound — same convention
// every other tmuxctl method that talks about a "named thing that does
// not exist on this server" upholds (cf. ShowBuffer's wrapping of "no
// buffer NAME"). tmux phrases the missing-file case as "<path>: No such
// file or directory" or "can't open <path>" depending on the version
// and the kind of failure; translate both into the same typed sentinel
// so callers can errors.Is into errs.ErrSessionNotFound regardless of
// which variant tmux happened to emit.
//
// Reusing ErrSessionNotFound (rather than introducing a new sentinel)
// keeps the JSON-RPC wire surface stable: the dispatcher already maps
// it to -32000, MCP clients already branch on that code for "the
// named thing does not exist on this server", and adding a parallel
// ErrFileNotFound would force every existing client to grow a second
// case for the same conceptual outcome.
func (c *Controller) SourceFile(ctx context.Context, path string, quiet bool) error {
	if path == "" {
		return errors.New("path required")
	}
	args := []string{"source-file"}
	if quiet {
		args = append(args, "-q")
	}
	args = append(args, path)
	if _, err := c.run(ctx, args...); err != nil {
		msg := strings.ToLower(err.Error())
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			(strings.Contains(msg, "no such file") ||
				strings.Contains(msg, "can't open") ||
				strings.Contains(msg, "file not found")) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

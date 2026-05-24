package tmuxctl

import (
	"context"
	"errors"
	"fmt"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// SaveBuffer returns the raw bytes of a tmux paste buffer by driving
// `tmux save-buffer - [-b NAME]`. The dash positional argument is the
// tmux convention for "write to stdout" — semantically equivalent to
// show-buffer for a buffer that fits in memory, but distinct in
// intent: callers reaching for save_buffer are signalling "I want the
// canonical, untruncated payload" so the JSON-RPC handler can opt
// into stricter response-size handling (see
// internal/server/tools_save_buffer.go).
//
// When name is empty `-b` is omitted and tmux writes the
// most-recently-added buffer, matching the CLI default and the
// existing ShowBuffer behaviour. When name is non-empty `-b NAME` is
// appended so the caller pins a specific buffer.
//
// A missing buffer surfaces as a wrapped errs.ErrSessionNotFound so
// the JSON-RPC dispatcher maps it to CodeSessionNotFound — the same
// sentinel ShowBuffer uses, so a client switching between the two
// tools sees a stable "the named thing does not exist" code
// regardless of which path produced the error. Validation of name's
// shape is left to the boundary layer (the regex/length check on the
// JSON-RPC side); tmux itself is the source of truth for which
// buffer names exist.
func (c *Controller) SaveBuffer(ctx context.Context, name string) (string, error) {
	args := []string{"save-buffer"}
	if name != "" {
		args = append(args, "-b", name)
	}
	// Trailing positional `-` is the tmux idiom for "write to stdout";
	// without it tmux would interpret the next argument as a file path
	// and create a file on disk, which is decisively not what an
	// agent-facing JSON-RPC handler wants.
	args = append(args, "-")
	out, err := c.run(ctx, args...)
	if err != nil {
		// run() already wraps "session not found" stderr shapes; the
		// "no buffer" / "unknown buffer" phrasing is buffer-specific
		// and lives next to the existing ShowBuffer mapping so both
		// read paths agree on the sentinel.
		if !errors.Is(err, errs.ErrSessionNotFound) && isBufferMissingMsg(err.Error()) {
			return "", fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return "", err
	}
	return out, nil
}

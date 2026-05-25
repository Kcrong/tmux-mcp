package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// SourceBuffer wraps `tmux source-buffer [-b NAME]`. tmux's source-buffer
// reads the named paste buffer (or the most-recently-added buffer when
// no `-b NAME` is supplied) and feeds the contents to tmux's command
// parser as a sequence of commands — the same parser that processes
// lines from `~/.tmux.conf` or `tmux source-file`. It is the on-server
// counterpart to source-file: agents can stage dynamic config edits in
// a paste buffer (via set_buffer / load_buffer) and then apply them
// without writing a file to disk.
//
// When name is empty no `-b` flag is appended and tmux picks the
// most-recently-added buffer (matching the CLI default — `tmux
// source-buffer` with no arguments). When name is non-empty it is
// passed verbatim as `-b NAME`; the boundary (server tool) is
// responsible for the up-front regex/length check, just like the
// pattern in ShowBuffer.
//
// Error mapping. The two outcomes that map to typed sentinels are:
//
//   - "no buffer <name>" / "unknown buffer" → wrapped errs.ErrSessionNotFound
//     so the JSON-RPC dispatcher emits CodeSessionNotFound, mirroring
//     ShowBuffer's contract for the same condition.
//   - "no server running" / "error connecting" / "No such file or
//     directory" (the headless-server stderr triplet ListBuffers /
//     ListSessions already handle uniformly) → wrapped
//     errs.ErrSessionNotFound. There are no buffers without a server,
//     so "the named buffer is not on this server" is a faithful
//     description of the failure regardless of which exact phrase tmux
//     emitted.
//
// Everything else — most importantly tmux's "unknown command" / parse
// failures from a malformed buffer body — surfaces verbatim through
// run() (CodeInternal). Those are user-input errors against the
// command parser, not a missing-buffer case, and conflating them with
// ErrSessionNotFound would break the wire contract clients rely on
// for "the thing you asked for does not exist".
func (c *Controller) SourceBuffer(ctx context.Context, name string) error {
	args := []string{"source-buffer"}
	if name != "" {
		args = append(args, "-b", name)
	}
	_, err := c.run(ctx, args...)
	if err == nil {
		return nil
	}
	// Already-typed errors from run() (e.g. context cancellation, the
	// "session not found" sentinel for a target we never set) flow
	// through unchanged so we don't double-wrap.
	if errors.Is(err, errs.ErrSessionNotFound) {
		return err
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	// Headless server: there are no buffers without a tmux daemon, so
	// every variant of "I can't reach the server" maps to the same
	// "buffer not found" wire code. Mirrors ListBuffers.
	if strings.Contains(lower, "no server running") ||
		strings.Contains(lower, "error connecting") ||
		strings.Contains(lower, "no such file or directory") {
		return fmt.Errorf("%s: %w", msg, errs.ErrSessionNotFound)
	}
	if isBufferMissingMsg(msg) {
		return fmt.Errorf("%s: %w", msg, errs.ErrSessionNotFound)
	}
	return err
}

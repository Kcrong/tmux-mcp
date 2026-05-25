package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// LoadBuffer wraps `tmux load-buffer -b NAME -` (read payload from
// stdin) so callers can stream a clipboard-style snippet into a tmux
// paste buffer without forcing the bytes through a shell argv. The
// behaviour matches SetBuffer field-for-field — same `name` / `append`
// semantics, same auto-name resolution path — but the payload travels
// over the child's stdin pipe instead of as a positional argument.
//
// Why two entry points (LoadBuffer here, SetBuffer in buffers.go) for
// what looks like the same operation? Because tmux's `set-buffer DATA`
// places the entire payload on the command line and that path is
// bounded by the OS argv length limit (ARG_MAX — ~128 KiB on Linux,
// ~256 KiB on macOS). A 5 KB clipboard snippet is comfortably below
// that ceiling, but a screenful of UTF-8 source code with embedded
// images, a multi-megabyte stack-trace dump, or a dense logfile slice
// will exceed it on Linux long before tmux's own 1 MiB buffer cap is
// reached. The kernel either truncates the exec or refuses it
// outright, and the operator sees a confusing
// "argument list too long" error from Go's os/exec rather than a
// clean tmux-side rejection. `load-buffer -` reads from stdin, which
// has no such cap — we just write the bytes to a pipe and close it,
// and tmux drains the pipe at its own pace. This is the only
// guaranteed-large-payload path tmux exposes, so when a caller wants
// to inject a buffer payload via a discrete tool call rather than
// going through `set-buffer`, this is the routine they reach for.
//
// Name resolution mirrors SetBuffer's: when name is non-empty tmux
// honours `-b NAME` verbatim and we echo it back without consulting
// list-buffers; when name is empty we run the same
// resolveLatestBufferName lookup SetBuffer uses to recover the
// auto-assigned `bufferN`.
//
// appendMode emulates tmux's `set-buffer -a` semantics. tmux's
// `load-buffer` only grew its own `-a` flag in 3.5, but tmux-mcp
// supports tmux 3.0+ — so on the tmux 3.4 baseline (the version
// shipped in our CI image) `load-buffer -a` is rejected with
// "unknown flag -a". To keep the surface uniform across versions we
// emulate append on top of the simpler load-buffer primitive: when
// appendMode is true and the target buffer already exists, we run
// `show-buffer -b NAME` to recover the existing payload, concatenate
// it with `data`, and feed the combined blob through a single
// `load-buffer -b NAME -`. When the target buffer does NOT exist —
// or no name was pinned and the server has no buffers yet — we fall
// through to a regular non-append load, mirroring tmux's
// "append-creates-when-missing" contract. The trade-off is a
// transient memory bump equal to the existing buffer's size; that is
// bounded by the MCP boundary's 1 MiB cap so it never balloons
// beyond what an agent's clipboard snippet would already cost.
//
// data is forwarded byte-for-byte over stdin; an empty string is
// allowed and creates (or replaces with) an empty buffer, matching
// tmux's behaviour for an empty stdin piped into `load-buffer -`.
func (c *Controller) LoadBuffer(ctx context.Context, data, name string, appendMode bool) (string, error) {
	if appendMode {
		return c.loadBufferAppend(ctx, data, name)
	}
	return c.loadBufferReplace(ctx, data, name)
}

// loadBufferReplace runs the simple non-append path: stream `data`
// through `tmux load-buffer -b NAME -` (or `tmux load-buffer -` for
// the auto-name case) and resolve the assigned name. This is the
// hot path — every caller that does not request append lands here —
// so it stays as small as possible.
func (c *Controller) loadBufferReplace(ctx context.Context, data, name string) (string, error) {
	args := []string{"load-buffer"}
	if name != "" {
		args = append(args, "-b", name)
	}
	// `-` tells tmux to read the buffer payload from stdin; the
	// runWithStdin helper writes data to the child's pipe and closes
	// it so tmux sees EOF and stops reading.
	args = append(args, "-")
	if _, err := c.runWithStdin(ctx, data, args...); err != nil {
		return "", err
	}
	if name != "" {
		// Caller pinned the name; tmux honours it verbatim, so there
		// is nothing to look up. Skipping the list-buffers round trip
		// also keeps the path side-effect-free for the most common
		// agent pattern (write under a known name, read it back).
		return name, nil
	}
	resolved, err := c.resolveLatestBufferName(ctx)
	if err != nil {
		return "", fmt.Errorf("load-buffer: resolve auto name: %w", err)
	}
	return resolved, nil
}

// loadBufferAppend emulates `tmux load-buffer -a` for the tmux 3.0+
// versions we support. The flag arrived in tmux 3.5; on older
// releases load-buffer rejects `-a` outright, so rather than gate
// behaviour on a runtime version probe we always use the
// show-buffer + load-buffer pair — it is correct on every supported
// version and the extra round-trip is bounded by the MCP boundary's
// 1 MiB cap on `data`.
//
// When the target buffer is missing (caller-pinned name with no
// existing entry, or auto-name path against an empty server),
// tmux's documented contract is "append silently creates the
// buffer". We honour that here: a missing target collapses to a
// plain replace — which, for an empty name, lets the auto-counter
// mint a fresh `bufferN` exactly like the tmux 3.5 native flag would.
func (c *Controller) loadBufferAppend(ctx context.Context, data, name string) (string, error) {
	target := name
	if target == "" {
		// Auto-name append: tmux's `set-buffer -a` (without -b)
		// extends the most-recently-added buffer. resolveLatest...
		// returns ErrSessionNotFound-shape when no buffers exist —
		// which we treat as "nothing to append to" and fall through
		// to a regular replace below.
		latest, err := c.resolveLatestBufferName(ctx)
		if err != nil {
			// resolveLatestBufferName surfaces "set-buffer succeeded
			// but list-buffers returned empty output" only when
			// there literally is no buffer table yet; that is the
			// auto-name "first write" path, and a fresh load is the
			// right behaviour. Any other error (a tmux exec failure
			// while we're trying to peek at the list) bubbles up so
			// the caller sees the real problem.
			if isEmptyBufferListErr(err) {
				return c.loadBufferReplace(ctx, data, "")
			}
			return "", fmt.Errorf("load-buffer: resolve append target: %w", err)
		}
		target = latest
	}
	// Read the existing payload so we can concatenate. show-buffer
	// against a missing name surfaces ErrSessionNotFound, which we
	// flatten to "no prefix" so the load below behaves like a fresh
	// write — matching tmux's "append-creates-when-missing"
	// behaviour.
	existing, err := c.ShowBuffer(ctx, target)
	if err != nil {
		if !errors.Is(err, errs.ErrSessionNotFound) {
			return "", fmt.Errorf("load-buffer: read append target: %w", err)
		}
		existing = ""
	}
	return c.loadBufferReplace(ctx, existing+data, target)
}

// isEmptyBufferListErr reports whether err carries the specific
// "list-buffers returned empty output" diagnostic resolveLatestBufferName
// emits when the server has no buffers yet. Substring matching keeps
// us aligned with the existing error string without forcing a typed
// sentinel for what is really just a "nothing to append to" signal —
// resolveLatestBufferName is the only producer of this exact phrase
// and it is internal to this package.
func isEmptyBufferListErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "list-buffers") && strings.Contains(msg, "empty output")
}

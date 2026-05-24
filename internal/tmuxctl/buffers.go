package tmuxctl

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// TODO(buffer-tools): a follow-up PR (feat/buffer-tools) introduces
// ListBuffers / ShowBuffer here. SetBuffer ships first because it has
// no dependency on either — it just writes a single buffer and returns
// the resolved name. When the broader buffer interface lands, this
// file will gain the matching reader/enumerator helpers; keep them in
// the same file so the buffer-related controller surface stays
// self-contained.

// SetBuffer wraps `tmux set-buffer DATA` (with optional `-b NAME` and
// `-a` for append). data is stored verbatim — tmux accepts arbitrary
// bytes, including the empty buffer — so the boundary layer is the
// only place that enforces a length cap.
//
// Name resolution. tmux assigns "bufferN" automatically when no
// `-b NAME` is supplied; older tmux releases (3.4 in our CI image)
// don't accept `-P` on set-buffer (the flag exists on display-message
// and a few other commands but not here), and stderr stays empty on
// success, so capturing the auto-name from stderr is unreliable across
// the versions we support. We therefore run a follow-up
// `list-buffers -F '#{buffer_created} #{buffer_name}'` and pick the
// most-recently-created entry — `buffer_created` is a unix timestamp
// at second granularity, with the auto-counter `bufferN` itself
// monotonically increasing, so we break ties by the lexically-largest
// `bufferN` and (for safety) fall back to the largest numeric suffix
// when both ordering hints disagree. When `name` is non-empty we skip
// the lookup entirely — tmux honours `-b NAME` verbatim and the
// resolved name is exactly what the caller passed.
//
// appendMode maps to tmux's `-a` flag, which concatenates onto an
// existing buffer (or creates a new one with that name when nothing
// exists yet — tmux does NOT error in that case, mirroring the
// boundary-layer documentation).
//
// A genuine fork/exec failure or unexpected tmux error surfaces
// verbatim through run(); the caller wraps with fmt.Errorf("%w") at
// the JSON-RPC layer so MCP clients see a stable error code.
func (c *Controller) SetBuffer(ctx context.Context, data, name string, appendMode bool) (string, error) {
	args := []string{"set-buffer"}
	if appendMode {
		args = append(args, "-a")
	}
	if name != "" {
		args = append(args, "-b", name)
	}
	// data is the final positional argument. tmux accepts an empty
	// string as a valid buffer payload (it really does store an
	// empty buffer), so we never substitute a placeholder here.
	args = append(args, data)
	if _, err := c.run(ctx, args...); err != nil {
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
		return "", fmt.Errorf("set-buffer: resolve auto name: %w", err)
	}
	return resolved, nil
}

// resolveLatestBufferName returns the name of the most-recently-created
// paste buffer the controller's tmux server is holding, used to
// recover the auto-assigned `bufferN` after a `set-buffer` with no
// `-b` flag. The selection runs against the live `list-buffers`
// output rather than caching a counter inside the controller because:
//
//   - tmux's auto-counter is server-wide, not per-call. Other clients
//     attached to the same server (rare in our private-socket setup,
//     but still possible) could mint buffers between our set and our
//     list, and a stale cached counter would attribute their buffer
//     to us.
//   - tmux occasionally garbage-collects buffers (configured
//     `buffer-limit`); a cached counter would point at a non-existent
//     name. Re-asking tmux is always correct.
//
// Sorting hierarchy (most-recent wins): largest `buffer_created`
// timestamp → lexically-largest name → largest numeric suffix on
// `bufferN`. The trio is overkill for a single-actor tmux server but
// makes the call deterministic regardless of how tmux orders its
// list-buffers output across versions.
func (c *Controller) resolveLatestBufferName(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "list-buffers", "-F", "#{buffer_created} #{buffer_name}")
	if err != nil {
		return "", err
	}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		// tmux silently lost the buffer between our set and our list
		// (e.g. another client called clear-history or set-buffer with
		// `-d` against the auto-name). Surface the empty result so the
		// caller can map it to a stable error rather than returning a
		// blank buffer name.
		return "", fmt.Errorf("set-buffer succeeded but list-buffers returned empty output")
	}
	type bufferEntry struct {
		created int64
		name    string
		// suffix is the integer parsed off "bufferN" when the entry
		// follows tmux's auto-name shape; -1 marks "not auto-named"
		// so caller-pinned names always sort below auto-names of the
		// same created/lex tier (we want the freshly-minted bufferN
		// to win the tie even if a long-lived caller-pinned name has
		// the same `buffer_created` second).
		suffix int
	}
	lines := strings.Split(out, "\n")
	entries := make([]bufferEntry, 0, len(lines))
	for _, line := range lines {
		// Format is "<unix-ts> <name>" — split on the first space so
		// names containing spaces (none in practice; tmux limits the
		// charset, but be defensive) round-trip intact. tmux
		// guarantees the timestamp is space-free.
		idx := strings.IndexByte(line, ' ')
		if idx <= 0 {
			return "", fmt.Errorf("set-buffer: malformed list-buffers line %q", line)
		}
		ts, perr := strconv.ParseInt(line[:idx], 10, 64)
		if perr != nil {
			return "", fmt.Errorf("set-buffer: parse buffer_created %q: %w", line[:idx], perr)
		}
		name := line[idx+1:]
		suffix := -1
		if rest, ok := strings.CutPrefix(name, "buffer"); ok {
			if n, perr := strconv.Atoi(rest); perr == nil && n >= 0 {
				suffix = n
			}
		}
		entries = append(entries, bufferEntry{created: ts, name: name, suffix: suffix})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		// Largest created first.
		if entries[i].created != entries[j].created {
			return entries[i].created > entries[j].created
		}
		// Then: largest auto-counter suffix wins.
		if entries[i].suffix != entries[j].suffix {
			return entries[i].suffix > entries[j].suffix
		}
		// Final tie-breaker: lexically larger name. Stable so
		// caller-pinned names that genuinely tie keep their relative
		// order from tmux's own output.
		return entries[i].name > entries[j].name
	})
	return entries[0].name, nil
}

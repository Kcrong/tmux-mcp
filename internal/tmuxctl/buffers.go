package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// BufferInfo describes a single tmux paste buffer as observed by
// `tmux list-buffers`.
//
// The fields are the subset of tmux buffer format variables that are
// actually useful to an agent that wants to enumerate clipboard-style
// snippets the controller's tmux server is currently holding. Each
// buffer's name is stable for its lifetime (tmux assigns "buffer0",
// "buffer1", … unless the caller pinned a custom name with
// `set-buffer -b NAME`); Size is the byte length of the buffer's
// payload; CreatedAt is the wall-clock time tmux first stored the
// buffer.
type BufferInfo struct {
	// Name is the tmux buffer name, echoed back verbatim so callers
	// can hand it to a follow-up show-buffer.
	Name string
	// Size is the byte length of the buffer's contents.
	Size int
	// CreatedAt is derived from tmux's #{buffer_created} (a unix
	// timestamp in seconds), normalised to UTC so the JSON-RPC layer
	// can emit a stable RFC3339 string regardless of the controller's
	// local timezone.
	CreatedAt time.Time
}

// listBuffersFormat matches the parsing in parseBufferLine — keep them
// in sync. tmux substitutes each #{...} variable and joins them with
// the literal '|' we placed between them; none of these tmux variables
// contain '|' in practice (the buffer name is restricted to safe
// characters and the size/created fields are numeric), so '|' is a
// safe field separator.
//
// We deliberately consume `#{buffer_created}` (raw unix timestamp)
// rather than `#{t:buffer_created}` (the human-readable form) so the
// JSON-RPC layer can emit a precise RFC3339 string in UTC; the
// human-format alternative is locale/timezone-dependent and would
// require fragile parsing on the way back out.
const listBuffersFormat = "#{buffer_name}|#{buffer_size}|#{buffer_created}"

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

// ListBuffers enumerates every paste buffer the controller's tmux
// server currently holds. The returned slice is empty (never nil) when
// no buffers exist — matching the `{"buffers": []}` envelope the
// JSON-RPC layer wants — so callers can iterate without a nil-guard.
//
// tmux exits cleanly with a zero status and no output when there are
// no buffers, so the empty case is not an error. A genuinely missing
// tmux server (no socket, no daemon) is rare in practice because
// list-buffers does not auto-spawn the server, but it is treated the
// same as "zero buffers" so callers don't have to special-case the
// fresh-controller path.
func (c *Controller) ListBuffers(ctx context.Context) ([]BufferInfo, error) {
	out, err := c.run(ctx, "list-buffers", "-F", listBuffersFormat)
	if err != nil {
		// A controller whose socket file does not yet exist (no
		// command has spun up the tmux server) reports "no server
		// running" / "error connecting". That is the steady-state
		// "nothing here" case for a freshly-constructed controller —
		// surface it as an empty list, mirroring ListSessions's
		// handling of the same condition.
		msg := err.Error()
		if strings.Contains(msg, "no server running") ||
			strings.Contains(msg, "error connecting") ||
			strings.Contains(msg, "No such file or directory") {
			return []BufferInfo{}, nil
		}
		return nil, err
	}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return []BufferInfo{}, nil
	}
	lines := strings.Split(out, "\n")
	buffers := make([]BufferInfo, 0, len(lines))
	for i, line := range lines {
		b, perr := parseBufferLine(line)
		if perr != nil {
			return nil, fmt.Errorf("list-buffers: line %d: %w", i+1, perr)
		}
		buffers = append(buffers, b)
	}
	return buffers, nil
}

// parseBufferLine splits one '|'-delimited row produced by
// listBuffersFormat into a BufferInfo. The format is fixed at the call
// site (above), so any drift in field count is a bug — reject it
// loudly rather than guess.
func parseBufferLine(line string) (BufferInfo, error) {
	const wantFields = 3
	fields := strings.Split(line, "|")
	if len(fields) != wantFields {
		return BufferInfo{}, fmt.Errorf("expected %d '|'-separated fields, got %d in %q", wantFields, len(fields), line)
	}
	size, err := strconv.Atoi(fields[1])
	if err != nil {
		return BufferInfo{}, fmt.Errorf("buffer_size %q: %w", fields[1], err)
	}
	createdUnix, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return BufferInfo{}, fmt.Errorf("buffer_created %q: %w", fields[2], err)
	}
	return BufferInfo{
		Name:      fields[0],
		Size:      size,
		CreatedAt: time.Unix(createdUnix, 0).UTC(),
	}, nil
}

// isBufferMissingMsg reports whether stderr text from tmux indicates
// the targeted buffer does not exist. tmux phrases this as
// "no buffer <name>" (and historically "unknown buffer"); detect both
// so callers can rely on errs.ErrSessionNotFound regardless of the
// version on PATH.
//
// We deliberately reuse errs.ErrSessionNotFound for the "named buffer
// not found" case rather than introducing a new sentinel: the JSON-RPC
// layer already maps it to a stable wire code (-32000), MCP clients
// already branch on that code for "the named thing does not exist on
// this server", and adding a parallel ErrBufferNotFound would force
// every existing client to grow a second case for the same conceptual
// outcome.
func isBufferMissingMsg(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "no buffer") ||
		strings.Contains(m, "unknown buffer")
}

// ShowBuffer returns the raw text content of a tmux paste buffer.
// When name is empty the most-recently-added buffer is dumped (i.e.
// `tmux show-buffer` with no -b), matching the tmux CLI default; this
// is the common case for an agent that just called set-buffer in the
// preceding step. When name is non-empty `-b NAME` is appended so the
// caller can pin a specific buffer.
//
// A missing buffer surfaces as a wrapped errs.ErrSessionNotFound so
// the JSON-RPC dispatcher maps it to CodeSessionNotFound. The
// boundary intentionally does NOT validate name's shape here — tmux
// itself is the source of truth for which buffer names exist; the
// server-tool layer is responsible for the up-front regex/length
// guard against stray quoting / shell metachars.
func (c *Controller) ShowBuffer(ctx context.Context, name string) (string, error) {
	args := []string{"show-buffer"}
	if name != "" {
		args = append(args, "-b", name)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		if !errors.Is(err, errs.ErrSessionNotFound) && isBufferMissingMsg(err.Error()) {
			return "", fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return "", err
	}
	return out, nil
}

// DeleteBuffer drops a single named paste buffer from the controller's
// tmux server via `tmux delete-buffer -b NAME`. The boundary always
// requires `name` — tmux's bare `delete-buffer` (no -b) deletes the
// most-recently-added buffer, but exposing that "delete the last thing
// you stored" path through a programmatic agent invites accidental
// destruction of buffers another caller just minted. Forcing the name
// keeps the operation deterministic from the caller's point of view.
//
// A missing buffer surfaces as a wrapped errs.ErrSessionNotFound so
// the JSON-RPC dispatcher maps it to CodeSessionNotFound — the same
// stable wire code ShowBuffer uses for the same conceptual outcome,
// so MCP clients can branch on a single sentinel for "the named
// buffer does not exist on this server" regardless of which read /
// mutate verb they reached for.
//
// The boundary deliberately does NOT validate name's shape here —
// tmux is the source of truth for which buffer names exist; the
// server-tool layer is responsible for the up-front regex/length
// guard against stray quoting / shell metachars.
func (c *Controller) DeleteBuffer(ctx context.Context, name string) error {
	if _, err := c.run(ctx, "delete-buffer", "-b", name); err != nil {
		if !errors.Is(err, errs.ErrSessionNotFound) && isBufferMissingMsg(err.Error()) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

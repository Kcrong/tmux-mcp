package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// SessionInfo is the structured metadata returned by
// [Controller.DescribeSession]. It is intentionally a flat value type so
// the JSON-RPC layer can serialise it directly.
type SessionInfo struct {
	// Name is the tmux session name, echoed back verbatim so callers
	// can correlate the response with the request.
	Name string
	// Windows is the number of windows in the session.
	Windows int
	// Panes is the total number of panes across every window of the
	// session (computed via `tmux list-panes -s -t <session>` and
	// counting the resulting lines, since #{session_panes} is not
	// available on every supported tmux version).
	Panes int
	// Width is the most-recent window width in columns.
	Width int
	// Height is the most-recent window height in rows.
	Height int
	// CreatedAt is the wall-clock time the session was created,
	// derived from tmux's #{session_created} (which is a unix
	// timestamp in seconds).
	CreatedAt time.Time
}

// describeFormat is the format string passed to `tmux display-message`.
// Each piece is separated by a literal '|' which is safe because none of
// these tmux variables contain '|' in practice. We deliberately use
// #{window_width} and #{window_height} (NOT #{client_width}/_height)
// because the latter only have values when a client is attached — for
// detached sessions, which is the default for tmux-mcp's own server,
// they come back empty. The window-* variants are the most-recent
// window size and work for both attached and detached sessions.
const describeFormat = "#{session_windows}|#{window_width}|#{window_height}|#{session_created}"

// DescribeSession returns structured metadata for a single named
// session. The caller is expected to validate the session name; this
// method just runs the tmux commands and parses the result.
//
// "Session not found" is surfaced as a wrapped errs.ErrSessionNotFound
// (via the underlying `has-session` call) so the JSON-RPC layer can map
// it to CodeSessionNotFound.
func (c *Controller) DescribeSession(ctx context.Context, name string) (SessionInfo, error) {
	if name == "" {
		return SessionInfo{}, errors.New("session name required")
	}
	// has-session is the canonical existence check — its stderr message
	// ("can't find session: <name>") is already recognised by
	// isSessionMissingMsg, so the wrapping happens automatically inside
	// run(). Doing this up front means the rest of this method can
	// assume the session exists; tmux's display-message silently
	// returns an empty line for unknown targets, which would otherwise
	// produce a confusing parse error instead of a typed sentinel.
	if _, err := c.run(ctx, "has-session", "-t", name); err != nil {
		return SessionInfo{}, err
	}
	// :0.0 anchors the format expansion to a real window+pane so the
	// window_width / window_height variables resolve. Without this
	// some tmux versions evaluate the variables against the empty
	// "current" client and return blanks even though the session
	// exists.
	out, err := c.run(ctx, "display-message", "-p", "-t", name+":0.0", "-F", describeFormat)
	if err != nil {
		return SessionInfo{}, err
	}
	parts := strings.Split(strings.TrimRight(out, "\n"), "|")
	if len(parts) != 4 {
		return SessionInfo{}, fmt.Errorf("describe %q: unexpected display-message output %q", name, out)
	}
	windows, err := strconv.Atoi(parts[0])
	if err != nil {
		return SessionInfo{}, fmt.Errorf("describe %q: parse session_windows %q: %w", name, parts[0], err)
	}
	width, err := strconv.Atoi(parts[1])
	if err != nil {
		return SessionInfo{}, fmt.Errorf("describe %q: parse window_width %q: %w", name, parts[1], err)
	}
	height, err := strconv.Atoi(parts[2])
	if err != nil {
		return SessionInfo{}, fmt.Errorf("describe %q: parse window_height %q: %w", name, parts[2], err)
	}
	createdUnix, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return SessionInfo{}, fmt.Errorf("describe %q: parse session_created %q: %w", name, parts[3], err)
	}
	// Pane count: list every pane in the session and count lines.
	// `#{session_panes}` would be a one-shot alternative but it was
	// added in a later tmux release than the minimum version this
	// project supports, so we deliberately go through list-panes.
	panesOut, err := c.run(ctx, "list-panes", "-s", "-t", name, "-F", "#{pane_id}")
	if err != nil {
		// The session existed at the has-session call above; if it is
		// gone now, surface the typed sentinel so callers still get a
		// clean CodeSessionNotFound on a TOCTOU race.
		if errors.Is(err, errs.ErrSessionNotFound) {
			return SessionInfo{}, err
		}
		return SessionInfo{}, fmt.Errorf("describe %q: list-panes: %w", name, err)
	}
	panes := 0
	for _, line := range strings.Split(strings.TrimRight(panesOut, "\n"), "\n") {
		if line != "" {
			panes++
		}
	}
	return SessionInfo{
		Name:      name,
		Windows:   windows,
		Panes:     panes,
		Width:     width,
		Height:    height,
		CreatedAt: time.Unix(createdUnix, 0).UTC(),
	}, nil
}

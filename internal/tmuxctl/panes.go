package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// Pane describes a single tmux pane as observed by `tmux list-panes`.
//
// The fields are the subset of tmux pane format variables that are
// actually useful for an agent that wants to retarget a subsequent
// send_keys / capture call to a non-default pane.
type Pane struct {
	// ID is the tmux-internal pane identifier (e.g. "%0"). Stable for
	// the lifetime of the pane and unique across the whole tmux server.
	ID string
	// Title is the pane title (often the process name, e.g. "vim").
	Title string
	// SessionWin is the "session_name:window_index" pair (e.g. "demo:0")
	// — combined with Index it forms the canonical "session:window.pane"
	// target string.
	SessionWin string
	// Index is the pane index within the window (0-based).
	Index int
	// Active reports whether this pane is the currently focused pane
	// of its window.
	Active bool
	// Width is the pane width in columns.
	Width int
	// Height is the pane height in rows.
	Height int
}

// listPanesFormat matches the parsing in parsePaneLine — keep them in
// sync. tmux substitutes each #{...} variable and joins them with the
// literal tab characters we placed between them.
const listPanesFormat = "#{pane_id}\t#{pane_title}\t#{session_name}:#{window_index}\t#{pane_index}\t#{pane_active}\t#{pane_width}\t#{pane_height}"

// ListPanes enumerates every pane visible to this controller's tmux
// server. When session is non-empty the listing is scoped to that
// session; otherwise every pane on the server is returned (`-a`).
//
// A typed errs.ErrSessionNotFound is returned (wrapped) when tmux
// reports the targeted session does not exist, so the JSON-RPC layer
// can map that to CodeSessionNotFound.
func (c *Controller) ListPanes(ctx context.Context, session string) ([]Pane, error) {
	args := []string{"list-panes", "-F", listPanesFormat}
	if session != "" {
		args = append(args, "-t", session)
	} else {
		// -a means "every pane on the server" — only useful when no
		// specific session was requested.
		args = append(args, "-a")
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		// tmux list-panes -t <session> rejects an unknown session with
		// "can't find window: <session>" because -t accepts a window
		// target, not a session target. Translate that into the same
		// typed sentinel run() emits for "session not found", so the
		// JSON-RPC dispatcher can map it to CodeSessionNotFound just
		// like the other tools.
		if session != "" && !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find window") {
			return nil, fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	lines := strings.Split(out, "\n")
	panes := make([]Pane, 0, len(lines))
	for i, line := range lines {
		p, perr := parsePaneLine(line)
		if perr != nil {
			return nil, fmt.Errorf("list-panes: line %d: %w", i+1, perr)
		}
		panes = append(panes, p)
	}
	return panes, nil
}

// parsePaneLine splits one tab-delimited row produced by listPanesFormat
// into a Pane. The format is fixed at the call site (above), so any
// drift in field count is a bug — reject it loudly rather than guess.
func parsePaneLine(line string) (Pane, error) {
	const wantFields = 7
	fields := strings.Split(line, "\t")
	if len(fields) != wantFields {
		return Pane{}, fmt.Errorf("expected %d tab-separated fields, got %d in %q", wantFields, len(fields), line)
	}
	idx, err := strconv.Atoi(fields[3])
	if err != nil {
		return Pane{}, fmt.Errorf("pane_index %q: %w", fields[3], err)
	}
	width, err := strconv.Atoi(fields[5])
	if err != nil {
		return Pane{}, fmt.Errorf("pane_width %q: %w", fields[5], err)
	}
	height, err := strconv.Atoi(fields[6])
	if err != nil {
		return Pane{}, fmt.Errorf("pane_height %q: %w", fields[6], err)
	}
	// tmux emits "1" for active panes and "0" otherwise.
	active := strings.TrimSpace(fields[4]) == "1"
	return Pane{
		ID:         fields[0],
		Title:      fields[1],
		SessionWin: fields[2],
		Index:      idx,
		Active:     active,
		Width:      width,
		Height:     height,
	}, nil
}

// SelectPane makes target the active pane of its window. target uses
// tmux's "session:window.pane" form (e.g. "demo:0.1"). An empty target
// is rejected up front because tmux would otherwise act on whatever
// pane it considers current — almost never what the caller wanted.
//
// A missing session surfaces as errs.ErrSessionNotFound (wrapped) so
// the JSON-RPC dispatcher can return CodeSessionNotFound.
func (c *Controller) SelectPane(ctx context.Context, target string) error {
	if target == "" {
		return errors.New("target required")
	}
	_, err := c.run(ctx, "select-pane", "-t", target)
	if err != nil {
		// run() already wraps errs.ErrSessionNotFound when tmux's
		// stderr says the session is missing; preserve that behaviour
		// for select-pane targets that point at a nonexistent session.
		return err
	}
	return nil
}

// Compile-time check that we still depend on the typed sentinel — if
// errs.ErrSessionNotFound is removed or renamed, this package fails to
// build instead of silently dropping the typed error path.
var _ = errs.ErrSessionNotFound

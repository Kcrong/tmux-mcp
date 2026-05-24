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

// ClientInfo describes a single tmux client (i.e. an attached terminal)
// as observed by `tmux list-clients`. The fields cover the format
// variables most useful to an agent that wants to know which terminals
// are currently driving a session — TTY path for kernel-level
// inspection, session name for routing follow-up calls, terminal
// dimensions for layout-aware logic, the read-only flag so a caller
// can avoid sending keys to a client that would refuse them, and the
// creation timestamp for ordering / staleness checks.
type ClientInfo struct {
	// TTY is the absolute path of the controlling terminal device the
	// client is attached through (e.g. "/dev/pts/3"). Stable for the
	// lifetime of the client.
	TTY string
	// Session is the tmux session name this client is currently
	// attached to. Empty when tmux reports the field empty (rare, but
	// possible for a client that just attached and has not yet been
	// associated with a session).
	Session string
	// Term is the TERM string the client advertised when it attached
	// (e.g. "xterm-256color", "screen.tmux"). Useful for callers that
	// need to know what kind of terminal is driving the session.
	Term string
	// Width is the client's terminal width in columns at the moment
	// of the listing.
	Width int
	// Height is the client's terminal height in rows at the moment of
	// the listing.
	Height int
	// ReadOnly reports whether the client was attached read-only
	// (`tmux attach -r`). Read-only clients still count toward the
	// listing but cannot drive the session through send-keys.
	ReadOnly bool
	// CreatedAt is when the client first attached to the tmux server,
	// as reported by `#{t:client_created}` (RFC3339-formatted by the
	// parser below from tmux's seconds-since-epoch output).
	CreatedAt time.Time
}

// listClientsFormat matches the parsing in parseClientLine — keep them
// in sync. tmux substitutes each #{...} variable and joins them with
// the literal '|' between them. '|' is safe here because tmux pads the
// fields with simple values that never contain it (TTY paths, session
// names already restricted by the boundary, the numeric/boolean width
// /height/readonly flags, and the integer client_created timestamp).
//
// `#{t:client_created}` would render an already-formatted string but
// tmux's default format is locale-sensitive and varies across versions,
// which makes round-tripping unreliable. We ask for the raw seconds-
// since-epoch via `#{client_created}` instead and convert to RFC3339
// in the parser, so callers always see a stable timestamp regardless
// of the tmux build on PATH.
const listClientsFormat = "#{client_tty}|#{client_session}|#{client_termname}|#{client_width}|#{client_height}|#{client_readonly}|#{client_created}"

// ListClients enumerates every client (attached terminal) visible to
// this controller's tmux server. When session is non-empty the listing
// is scoped to clients attached to that specific session via `-t`;
// otherwise every client on the server is returned (no `-t`), matching
// the convention `tmux list-clients` uses on the CLI.
//
// Empty stdout (no clients attached) is treated as a successful empty
// listing — returns (nil, nil) — rather than an error. Callers can rely
// on the zero-length slice for a clean "no terminals attached" branch
// without having to substring-match "no clients" in stderr.
//
// A typed errs.ErrSessionNotFound is returned (wrapped) when tmux
// reports the targeted session does not exist, so the JSON-RPC layer
// can map that to CodeSessionNotFound. Other tmux failures pass
// through unchanged so the dispatcher surfaces them via CodeInternal.
func (c *Controller) ListClients(ctx context.Context, session string) ([]ClientInfo, error) {
	args := []string{"list-clients", "-F", listClientsFormat}
	if session != "" {
		args = append(args, "-t", session)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		// `tmux list-clients -t <session>` rejects an unknown session
		// with "can't find session: <name>" (which run() already
		// translates) on most builds, but some emit "can't find window"
		// because -t notionally accepts a window target — translate
		// that into the same typed sentinel so the JSON-RPC dispatcher
		// can map every "session not found" surface uniformly.
		msg := strings.ToLower(err.Error())
		if session != "" && !errors.Is(err, errs.ErrSessionNotFound) &&
			(strings.Contains(msg, "can't find window") ||
				strings.Contains(msg, "no server running")) {
			return nil, fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		// No clients attached — common for the headless servers
		// tmux-mcp owns. Return an empty slice (nil) so callers can
		// iterate without a separate branch.
		return nil, nil
	}
	lines := strings.Split(out, "\n")
	clients := make([]ClientInfo, 0, len(lines))
	for i, line := range lines {
		ci, perr := parseClientLine(line)
		if perr != nil {
			return nil, fmt.Errorf("list-clients: line %d: %w", i+1, perr)
		}
		clients = append(clients, ci)
	}
	return clients, nil
}

// parseClientLine splits one '|'-delimited row produced by
// listClientsFormat into a ClientInfo. The format is fixed at the call
// site (above), so any drift in field count is a bug — reject it
// loudly rather than guess.
func parseClientLine(line string) (ClientInfo, error) {
	const wantFields = 7
	fields := strings.Split(line, "|")
	if len(fields) != wantFields {
		return ClientInfo{}, fmt.Errorf("expected %d '|'-separated fields, got %d in %q", wantFields, len(fields), line)
	}
	width, err := strconv.Atoi(fields[3])
	if err != nil {
		return ClientInfo{}, fmt.Errorf("client_width %q: %w", fields[3], err)
	}
	height, err := strconv.Atoi(fields[4])
	if err != nil {
		return ClientInfo{}, fmt.Errorf("client_height %q: %w", fields[4], err)
	}
	// tmux emits "1" for read-only clients and "0" otherwise.
	readonly := strings.TrimSpace(fields[5]) == "1"
	// tmux's `#{client_created}` is the seconds-since-epoch the client
	// attached. Parse to an explicit time.Time so callers always work
	// with a typed, RFC3339-encodable value (the JSON-RPC layer above
	// formats it via time.Format(time.RFC3339)).
	secs, err := strconv.ParseInt(strings.TrimSpace(fields[6]), 10, 64)
	if err != nil {
		return ClientInfo{}, fmt.Errorf("client_created %q: %w", fields[6], err)
	}
	return ClientInfo{
		TTY:       fields[0],
		Session:   fields[1],
		Term:      fields[2],
		Width:     width,
		Height:    height,
		ReadOnly:  readonly,
		CreatedAt: time.Unix(secs, 0).UTC(),
	}, nil
}

// Compile-time check that we still depend on the typed sentinel — if
// errs.ErrSessionNotFound is removed or renamed, this package fails to
// build instead of silently dropping the typed error path.
var _ = errs.ErrSessionNotFound

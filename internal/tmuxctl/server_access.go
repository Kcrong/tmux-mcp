package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// serverAccessUserRE matches POSIX-portable usernames. tmux's
// `server-access` ultimately resolves the supplied USER through the
// host's passwd database, so any byte sequence accepted by the OS
// (`getpwnam`) would otherwise reach tmux's argv. We pin the validator
// to the conservative POSIX `name_regex` ([3.437]):
//
//	[a-z_][a-z0-9_-]*[$]?
//
// Excluding the trailing `$` (a Samba-only convention rarely used by
// real interactive accounts) and capping length below.
//
// The boundary refuses anything outside the regex with a clean
// "invalid username" error so a stray quote / shell metachar / NUL
// byte cannot reach `tmux server-access -a $USER` on a multi-tenant
// host. Allowing a wider set would be defensible (the OS already
// rejects unknown users with a clean stderr) but the regex form
// matches what every other tmux-mcp validator does for its
// argument-shape policy and keeps the surface uniform.
var serverAccessUserRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)

// maxServerAccessUserLen caps the username length at the POSIX
// `LOGIN_NAME_MAX` floor, which is 32 on every supported Linux kernel
// and macOS release. Going above this is almost certainly a bug — no
// real `getpwnam` entry on a tmux-supported host has a longer login
// name — so refusing the input here keeps tmux's argv bounded.
const maxServerAccessUserLen = 32

// ServerAccessEntry is a single row from `tmux server-access -l`. The
// list output is line-oriented:
//
//	username R/W
//	otheruser R
//
// where the trailing token is `R/W` (read+write) for users with full
// access and `R` for read-only entries. The owner of the running tmux
// daemon is printed without a trailing token (tmux omits the column
// for root / the server owner because their access is always granted
// and cannot be revoked); the parser preserves that shape by leaving
// `Permission` empty in those rows.
type ServerAccessEntry struct {
	// User is the OS username as printed by tmux. Trimmed of trailing
	// whitespace; never empty when the row was a real entry (blank
	// lines are dropped by the parser).
	User string `json:"user"`
	// Permission is the access token tmux printed alongside the
	// username. `R/W` for read+write, `R` for read-only, and the
	// empty string for the server owner (tmux omits the column on
	// that row). Tests should not assume any other token shape — if
	// tmux ever ships a third permission level, the parser surfaces
	// the new token verbatim so callers can still display it.
	Permission string `json:"permission"`
}

// validateServerAccessUser pins the per-call username policy used by
// every mutating ServerAccess* method. Empty is rejected up front
// (the user CLI is keyword-required for `-a`, `-d`, `-r`, `-w`), then
// the length and regex caps fire so a stray quote or shell metachar
// cannot reach tmux's argv. The function returns a plain error so the
// JSON-RPC layer can wrap it with `invalidParams` — it is not a
// `*rpcError` because the controller package must stay free of any
// `internal/server` dependency.
func validateServerAccessUser(user string) error {
	if user == "" {
		return errors.New("server_access: user required")
	}
	if len(user) > maxServerAccessUserLen {
		return fmt.Errorf(
			"server_access: user length %d out of range [1..%d]",
			len(user), maxServerAccessUserLen,
		)
	}
	if !serverAccessUserRE.MatchString(user) {
		return fmt.Errorf(
			"server_access: user %q must match %s",
			user, serverAccessUserRE.String(),
		)
	}
	return nil
}

// ServerAccessAdd wraps `tmux server-access -a USER`, granting the
// named OS user access to the controller's shared tmux socket. When
// `-w` is not also set tmux defaults the new entry to read-only; the
// JSON-RPC tool surfaces a separate `op=write` to flip the bit so
// callers compose Add + Write rather than relying on a magic combined
// flag.
//
// Validation runs at the controller boundary: the username must be
// non-empty, ≤ 32 bytes, and match the POSIX `name_regex`. Anything
// outside that envelope is refused before tmux is consulted.
//
// tmux's `server-access` requires the daemon to be running (the
// command is a server message, not a client-only configuration tweak).
// Callers are expected to issue an anchoring CreateSession or
// StartServer before the first Add — the controller does not auto-
// start the daemon to keep the side effects visible to the caller.
func (c *Controller) ServerAccessAdd(ctx context.Context, user string) error {
	if err := validateServerAccessUser(user); err != nil {
		return err
	}
	_, err := c.run(ctx, "server-access", "-a", user)
	return err
}

// ServerAccessDelete wraps `tmux server-access -d USER`, revoking the
// named user's access to the shared socket. tmux detaches any of the
// user's currently-attached clients as part of the call, so a delete
// against a live attached user is the right primitive for "kick this
// peer out".
//
// Validation matches ServerAccessAdd: non-empty, ≤ 32 bytes, POSIX
// name_regex. The boundary refuses bad input before tmux is consulted.
func (c *Controller) ServerAccessDelete(ctx context.Context, user string) error {
	if err := validateServerAccessUser(user); err != nil {
		return err
	}
	_, err := c.run(ctx, "server-access", "-d", user)
	return err
}

// ServerAccessReadOnly wraps `tmux server-access -r USER`, switching
// the named entry to read-only access. tmux requires the user to
// already exist in the access list (typically via a prior
// ServerAccessAdd); the boundary forwards tmux's stderr verbatim if
// not, so the caller sees the underlying "user not found in access
// list" diagnostic rather than a synthesised one.
func (c *Controller) ServerAccessReadOnly(ctx context.Context, user string) error {
	if err := validateServerAccessUser(user); err != nil {
		return err
	}
	_, err := c.run(ctx, "server-access", "-r", user)
	return err
}

// ServerAccessWrite wraps `tmux server-access -w USER`, switching the
// named entry to read+write access. Mirror of ServerAccessReadOnly:
// tmux requires the user to already exist in the access list, and the
// boundary forwards tmux's stderr verbatim on a missing entry so the
// caller can branch on the underlying diagnostic.
func (c *Controller) ServerAccessWrite(ctx context.Context, user string) error {
	if err := validateServerAccessUser(user); err != nil {
		return err
	}
	_, err := c.run(ctx, "server-access", "-w", user)
	return err
}

// ServerAccessList wraps `tmux server-access -l`, returning every
// access-list entry the running daemon currently holds. The output is
// parsed line-by-line into [ServerAccessEntry] structs:
//
//	username R/W   → {User: "username", Permission: "R/W"}
//	username R     → {User: "username", Permission: "R"}
//	owner          → {User: "owner",    Permission: ""}      // server owner row
//
// Empty stdout (the daemon's access list is empty) returns an empty
// slice, not nil, so callers can range over the result without a
// nil-check. Whitespace-only lines and blank stderr are likewise
// dropped.
//
// Headless behaviour. When the controller's tmux daemon is not yet
// running, tmux exits with `no server running on <socket>` (or
// `error connecting to <socket>` when the socket file is missing
// outright, or `server exited unexpectedly` for the transient race
// where the daemon disappeared mid-call). Each phrase is recognised
// here and collapsed to a clean `nil, nil` — the access list of a
// non-existent server is observationally identical to an empty access
// list, and surfacing the difference would push every caller to write
// the same "did the server even start?" branch.
//
// Anything else surfaces as a wrapped error so the JSON-RPC layer can
// map it to CodeInternal.
func (c *Controller) ServerAccessList(ctx context.Context) ([]ServerAccessEntry, error) {
	out, err := c.run(ctx, "server-access", "-l")
	if err != nil {
		// Mirror ListSessions's "headless server" handling. tmux phrases
		// the "no daemon" condition three different ways across the
		// supported version range, and at this layer they all mean the
		// same thing: zero entries.
		msg := err.Error()
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "no server running") ||
			strings.Contains(lower, "error connecting") ||
			strings.Contains(lower, "server exited unexpectedly") ||
			strings.Contains(lower, "no such file or directory") {
			return nil, nil
		}
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		// `server-access -l` printing a blank output means tmux has no
		// entries to report — return an empty slice so the caller can
		// range over it without a nil-check.
		return []ServerAccessEntry{}, nil
	}
	lines := strings.Split(out, "\n")
	entries := make([]ServerAccessEntry, 0, len(lines))
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// tmux separates the username and the permission token with a
		// run of whitespace (a single space on every supported version,
		// but `Fields` is robust against tab expansion or future
		// formatting tweaks).
		fields := strings.Fields(line)
		switch len(fields) {
		case 0:
			// `Fields` returns an empty slice only for whitespace-only
			// lines, which the trim above already rejected. The case
			// stays as a defensive guard so a future tmux that emits a
			// stray blank does not panic the parser.
			continue
		case 1:
			// Server-owner row. tmux omits the permission column on the
			// row that prints the daemon's owning user; preserve the
			// shape with an empty Permission so callers can still see
			// the username in the listing.
			entries = append(entries, ServerAccessEntry{User: fields[0]})
		default:
			// First field is the username; everything else is the
			// permission token. Real-world tmux only ever prints `R` or
			// `R/W`, but joining the tail with a single space leaves the
			// parser forward-compatible if a future tmux ships a
			// multi-word permission descriptor.
			entries = append(entries, ServerAccessEntry{
				User:       fields[0],
				Permission: strings.Join(fields[1:], " "),
			})
		}
	}
	return entries, nil
}

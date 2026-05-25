package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// DisplayPopupOptions describes a single `tmux display-popup` invocation.
// The boundary (the display_popup tool) is responsible for validating the
// shape of every field; this struct just shapes the spec into a form the
// controller can mechanically translate into tmux flags.
//
// Empty values mean "do not pass this flag" so the resulting argv stays
// minimal — tmux applies its own defaults for anything we omit (centred
// half-the-terminal sizing, default border, no environment overrides).
//
// Only ShellCommand is positional; every other field maps onto a
// flag the controller assembles in a stable order so the test harness
// can assert on the constructed argv without depending on Go's map
// iteration order.
type DisplayPopupOptions struct {
	// Target is the tmux pane target the popup is associated with
	// (e.g. "session", "session:window", "session:window.pane",
	// "%5"). Empty omits `-t` so tmux resolves the popup against the
	// active client's current pane. The boundary regex keeps stray
	// shell metachars out before this layer is reached.
	Target string
	// Title is the format string tmux substitutes into the popup
	// title bar (`-T`). tmux's format DSL is recursive, so the
	// boundary caps the length.
	Title string
	// BorderStyle is the tmux style spec applied to the popup
	// border (`-S`). Same recursion concern as Title.
	BorderStyle string
	// BorderLines selects the glyph set tmux uses to draw the
	// popup border (`-b`). tmux 3.4 accepts "single", "double",
	// "heavy", "simple", "rounded", "padded", "none". The boundary
	// forwards the value verbatim and lets tmux refuse anything
	// unknown so the controller does not need a per-tmux-version
	// allowlist.
	BorderLines string
	// StartDirectory is the cwd the popup's shell-command is
	// spawned in (`-d`). Must be absolute when set; the boundary
	// enforces that — passing a relative path through to tmux
	// would resolve against the daemon's cwd, which is rarely what
	// the caller meant.
	StartDirectory string
	// Width / Height accept either a percentage (e.g. "80%") or a
	// number of cells. Empty omits the flag and tmux defaults to
	// half the terminal size on each axis.
	Width  string
	Height string
	// X / Y position the popup. Same flag-omission contract as
	// Width / Height: empty leaves tmux's centred default in place.
	X string
	Y string
	// ShellCommand is the trailing positional argument tmux runs
	// inside the popup. Empty omits the positional so the popup
	// runs the user's default shell.
	ShellCommand string
	// Env is the set of `VAR=value` pairs passed to the popup
	// shell-command via `-e`. The controller emits one `-e
	// KEY=VALUE` flag per entry in deterministic (sorted) key
	// order so the constructed argv is stable across calls.
	Env map[string]string
	// NoBorder maps to `-B` (suppresses the popup border). When
	// true, tmux ignores any BorderLines/BorderStyle the caller
	// also supplied — that is tmux's documented behaviour, not a
	// quirk we paper over.
	NoBorder bool
	// CloseOnExit maps to `-C` — closes the popup as soon as the
	// shell-command exits (regardless of exit code).
	CloseOnExit bool
	// CloseOnZeroExit maps to `-E` — closes the popup only when
	// the shell-command exits successfully (zero exit code).
	CloseOnZeroExit bool
	// Centered maps to `-r`. Newer tmux builds (post-3.4) accept
	// it as a shorthand for "centre the popup on the active
	// client". Older builds reject the flag at the daemon, which
	// the controller surfaces as a plain CodeInternal — the
	// boundary documents this so callers running against an older
	// tmux can avoid the flag.
	Centered bool
}

// DisplayPopup wraps `tmux display-popup`. It assembles the argv from a
// DisplayPopupOptions value, dispatches to the controller's tmux
// daemon, and returns nil on success. The popup is fire-and-forget at
// this layer — tmux either rendered the popup (or queued it for the
// next client refresh) or it didn't, and the boundary turns that into
// a flat `{"opened": true}` ack.
//
// Argv ordering. The controller emits flags in a stable order so the
// test harness can assert on the constructed argv without depending on
// Go's map iteration semantics. The order mirrors the way an operator
// would type the command on the CLI: boolean flags first
// (-B / -C / -E / -r), then keyed flags in an alphabetical-ish order
// (-T / -S / -b / -d / -e / -h / -w / -x / -y / -t), and finally the
// positional shell-command.
//
// Sentinel mapping. tmux's display-popup rejects an unknown target
// with "can't find session" / "can't find window" / "can't find pane"
// depending on which half of the target it blamed; the controller
// translates each phrasing into the typed [errs.ErrSessionNotFound] so
// the JSON-RPC layer can map every missing-target surface to
// CodeSessionNotFound (-32000) uniformly. Other failures (no current
// client, unknown flag on an old tmux, etc.) propagate verbatim so
// the dispatcher surfaces them via CodeInternal.
func (c *Controller) DisplayPopup(ctx context.Context, opts DisplayPopupOptions) error {
	args := []string{"display-popup"}
	if opts.NoBorder {
		args = append(args, "-B")
	}
	if opts.CloseOnExit {
		args = append(args, "-C")
	}
	if opts.CloseOnZeroExit {
		args = append(args, "-E")
	}
	if opts.Centered {
		// `-r` is a tmux ≥ 3.5 addition. Older daemons reject
		// the flag with "command display-popup: unknown flag
		// -r"; the boundary documents this so callers can avoid
		// the flag on older tmux without being surprised.
		args = append(args, "-r")
	}
	if opts.Title != "" {
		args = append(args, "-T", opts.Title)
	}
	if opts.BorderStyle != "" {
		// `-S` sets the *border* style; `-s` (lowercase) sets
		// the popup-content style. The schema only exposes the
		// former because the latter has rarely-needed semantics
		// and would inflate the surface without a clear win.
		args = append(args, "-S", opts.BorderStyle)
	}
	if opts.BorderLines != "" {
		args = append(args, "-b", opts.BorderLines)
	}
	if opts.StartDirectory != "" {
		args = append(args, "-d", opts.StartDirectory)
	}
	if len(opts.Env) > 0 {
		// Sort the keys so the constructed argv is deterministic.
		// tmux applies the env entries in the order they appear on
		// the CLI, but we never depend on insertion order — the
		// stable iteration is purely for the test harness, which
		// asserts on the exact argv slice.
		keys := make([]string, 0, len(opts.Env))
		for k := range opts.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-e", k+"="+opts.Env[k])
		}
	}
	if opts.Height != "" {
		args = append(args, "-h", opts.Height)
	}
	if opts.Width != "" {
		args = append(args, "-w", opts.Width)
	}
	if opts.X != "" {
		args = append(args, "-x", opts.X)
	}
	if opts.Y != "" {
		args = append(args, "-y", opts.Y)
	}
	if opts.Target != "" {
		args = append(args, "-t", opts.Target)
	}
	if opts.ShellCommand != "" {
		// The shell-command is positional and tmux invokes it
		// via /bin/sh -c, so we forward it as a single argv
		// element to preserve the caller's quoting.
		args = append(args, opts.ShellCommand)
	}
	if _, err := c.run(ctx, args...); err != nil {
		// `tmux display-popup -t <missing>` rejects an unknown
		// target with one of three phrases depending on which
		// half tmux blamed:
		//   - "can't find session: NAME"
		//   - "can't find window: NAME:WIN"
		//   - "can't find pane: TARGET"
		// run() already wraps the first phrasing through
		// isSessionMissingMsg, so the wrapping below covers the
		// remaining two. Without this, a caller relying on
		// errs.ErrSessionNotFound to detect "the target does not
		// exist" would see a plain CodeInternal when tmux happened
		// to blame the window or pane half of the target.
		if !errors.Is(err, errs.ErrSessionNotFound) {
			lower := strings.ToLower(err.Error())
			if strings.Contains(lower, "can't find window") ||
				strings.Contains(lower, "can't find pane") {
				return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
			}
		}
		return err
	}
	return nil
}

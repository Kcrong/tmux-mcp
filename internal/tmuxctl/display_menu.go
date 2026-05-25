package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// DisplayMenuItem is a single row in a tmux menu. Name is the label
// rendered in the menu (and may be empty to draw a separator); Key is
// the optional single-key shortcut shown in brackets next to the row;
// Command is the tmux command run when the item is chosen. Items with
// an empty Name are passed to tmux as separators per the man page —
// tmux requires Key and Command to be omitted/empty in that case but
// our argv expansion still emits the empty triple so the alignment of
// later items is preserved.
type DisplayMenuItem struct {
	Name    string
	Key     string
	Command string
}

// DisplayMenuOpts mirrors the flag surface of `tmux display-menu`. Only
// fields the caller sets reach tmux's argv — empty strings and zero-
// valued ints are skipped so tmux falls back to its built-in defaults
// (status-line position, no border, etc).
//
// The Items slice is required and must contain at least one entry: tmux
// itself rejects an empty menu. Each item expands into the alternating
// `name key command` triple the man page calls for, with empty Key /
// Command rendered as the empty string (`""`) so tmux's positional
// parser keeps the alignment of later items.
type DisplayMenuOpts struct {
	TargetPane     string // -t TARGET-PANE
	TargetClient   string // -c TARGET-CLIENT
	Title          string // -T TITLE
	BorderLines    string // -b BORDER-LINES
	BorderStyle    string // -S BORDER-STYLE
	SelectedStyle  string // -H SELECTED-STYLE
	StartingChoice string // -C STARTING-CHOICE (string so callers may pass "0", "first", etc.)
	X              string // -x POSITION
	Y              string // -y POSITION
	NoCallbacks    bool   // -O
	Items          []DisplayMenuItem
}

// DisplayMenu wraps `tmux display-menu [-O] [-b BORDER-LINES] [-c
// TARGET-CLIENT] [-C STARTING-CHOICE] [-H SELECTED-STYLE] [-S
// BORDER-STYLE] [-T TITLE] [-t TARGET-PANE] [-x POSITION] [-y POSITION]
// name key command ...`.
//
// tmux's display-menu needs an attached client to draw on: the headless
// tmux servers tmux-mcp typically owns will surface tmux's `no current
// client` diagnostic verbatim. This boundary forwards that error rather
// than synthesising a sentinel because the caller may legitimately
// pass `target_client` to scope the draw call to a specific TTY.
//
// Up-front validation rejects an empty Items slice (tmux requires at
// least one item) and any item whose Name is the empty string but
// whose Key/Command are also empty would be a no-op separator with no
// way to get past it. We force every item to carry a non-empty Name so
// agents do not silently produce a menu the user cannot navigate.
//
// "can't find client/pane/window" stderr from tmux maps to the typed
// errs.ErrSessionNotFound sentinel so the JSON-RPC dispatcher can map
// the failure uniformly to CodeSessionNotFound (-32000).
func (c *Controller) DisplayMenu(ctx context.Context, opts DisplayMenuOpts) error {
	if len(opts.Items) == 0 {
		return errors.New("display-menu: at least one item required")
	}
	for i, it := range opts.Items {
		if it.Name == "" {
			return fmt.Errorf("display-menu: item[%d].name must not be empty", i)
		}
	}

	// argv assembly mirrors the tmux man-page ordering so the resulting
	// command line is easy to compare against `tmux display-menu -h` if
	// a regression surfaces in CI.
	args := []string{"display-menu"}
	if opts.NoCallbacks {
		args = append(args, "-O")
	}
	if opts.BorderLines != "" {
		args = append(args, "-b", opts.BorderLines)
	}
	if opts.TargetClient != "" {
		args = append(args, "-c", opts.TargetClient)
	}
	if opts.StartingChoice != "" {
		args = append(args, "-C", opts.StartingChoice)
	}
	if opts.SelectedStyle != "" {
		args = append(args, "-H", opts.SelectedStyle)
	}
	if opts.BorderStyle != "" {
		args = append(args, "-S", opts.BorderStyle)
	}
	if opts.Title != "" {
		args = append(args, "-T", opts.Title)
	}
	if opts.TargetPane != "" {
		args = append(args, "-t", opts.TargetPane)
	}
	if opts.X != "" {
		args = append(args, "-x", opts.X)
	}
	if opts.Y != "" {
		args = append(args, "-y", opts.Y)
	}
	for _, it := range opts.Items {
		// Each menu row is a positional triple: name, key, command.
		// tmux's parser is strict about the count — even a separator
		// (name starting with "-") still consumes three positional
		// tokens, so the empty Key/Command are forwarded as the empty
		// string rather than being elided.
		args = append(args, it.Name, it.Key, it.Command)
	}

	if _, err := c.run(ctx, args...); err != nil {
		// `tmux display-menu` phrases its missing-target diagnostics as
		// "can't find client/pane/window" — translate any of those into
		// the typed sentinel so the JSON-RPC layer maps them uniformly.
		if !errors.Is(err, errs.ErrSessionNotFound) {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "can't find client") ||
				strings.Contains(msg, "can't find pane") ||
				strings.Contains(msg, "can't find window") {
				return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
			}
		}
		return err
	}
	return nil
}

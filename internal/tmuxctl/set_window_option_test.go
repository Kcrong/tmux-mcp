package tmuxctl

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestBuildSetWindowOptionArgs covers the pure assembly of the argv
// passed to `tmux set-window-option` for every flag combination.
// Keeping this as a table-driven test (with no live tmux dependency)
// lets us pin every flag/order detail without paying the cost of
// spinning up a controller per row.
func TestBuildSetWindowOptionArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		target       string
		opt          string
		value        string
		appendValue  bool
		formatExpand bool
		global       bool
		allowMissing bool
		unset        bool
		want         []string
		wantErr      string
	}{
		{
			name:   "happy path with target",
			target: "demo:0",
			opt:    "synchronize-panes",
			value:  "on",
			want:   []string{"set-window-option", "-t", "demo:0", "synchronize-panes", "on"},
		},
		{
			name:   "global without target",
			opt:    "mode-keys",
			value:  "vi",
			global: true,
			want:   []string{"set-window-option", "-g", "mode-keys", "vi"},
		},
		{
			name:        "append flag passes -a",
			target:      "demo:0",
			opt:         "pane-border-format",
			value:       "#{pane_index}",
			appendValue: true,
			want:        []string{"set-window-option", "-a", "-t", "demo:0", "pane-border-format", "#{pane_index}"},
		},
		{
			name:         "format-expand flag passes -F",
			target:       "demo:0",
			opt:          "window-status-format",
			value:        "#{window_index}",
			formatExpand: true,
			want:         []string{"set-window-option", "-F", "-t", "demo:0", "window-status-format", "#{window_index}"},
		},
		{
			name:         "allow-missing flag passes -q",
			target:       "demo:0",
			opt:          "synchronize-panes",
			value:        "off",
			allowMissing: true,
			want:         []string{"set-window-option", "-q", "-t", "demo:0", "synchronize-panes", "off"},
		},
		{
			name:   "unset suppresses VALUE and adds -u",
			target: "demo:0",
			opt:    "synchronize-panes",
			value:  "ignored-by-unset",
			unset:  true,
			want:   []string{"set-window-option", "-u", "-t", "demo:0", "synchronize-panes"},
		},
		{
			name:         "all flags compose in documented order",
			target:       "demo:0",
			opt:          "pane-border-format",
			value:        "#{pane_index}",
			appendValue:  true,
			formatExpand: true,
			global:       true,
			allowMissing: true,
			want: []string{
				"set-window-option", "-a", "-F", "-g", "-q",
				"-t", "demo:0", "pane-border-format", "#{pane_index}",
			},
		},
		{
			name:    "empty name rejected",
			value:   "on",
			wantErr: "option name required",
		},
		{
			name:  "empty value preserved verbatim (no placeholder)",
			opt:   "history-file",
			value: "",
			want:  []string{"set-window-option", "history-file", ""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := buildSetWindowOptionArgs(
				tc.target, tc.opt, tc.value,
				tc.appendValue, tc.formatExpand, tc.global, tc.allowMissing, tc.unset,
			)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q, got args %v", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("args mismatch\n got: %v\nwant: %v", got, tc.want)
			}
		})
	}
}

// TestSetWindowOption_HappyPath_SynchronizePanes drives the load-bearing
// integration path: setting the textbook window option
// `synchronize-panes` to `on` for an existing session, then probing
// `show-window-options -t SESSION:0 synchronize-panes` and asserting the
// echoed value. We pick `synchronize-panes` because it is a stable
// boolean in every supported tmux version and lives on the per-window
// table (not the session table) — exactly the gap the new tool fills.
func TestSetWindowOption_HappyPath_SynchronizePanes(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const session = "swo_happy"
	if err := c.CreateSession(ctx, SessionSpec{Name: session, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.SetWindowOption(ctx, session+":0", "synchronize-panes", "on",
		false, false, false, false, false); err != nil {
		t.Fatalf("SetWindowOption: %v", err)
	}

	// show-window-options echoes `synchronize-panes on` when the
	// override has been recorded; an empty stdout would mean the
	// option never landed (tmux's behaviour when no override is set
	// at the per-window level and -g is not supplied).
	out, err := c.run(ctx, "show-window-options", "-t", session+":0", "synchronize-panes")
	if err != nil {
		t.Fatalf("show-window-options: %v", err)
	}
	if !strings.Contains(out, "synchronize-panes on") {
		t.Fatalf("show-window-options output %q does not echo `synchronize-panes on`", out)
	}
}

// TestSetWindowOption_AppendStringListOption pins the `-a` (append)
// flag against `pane-border-format`, a string-list option whose values
// concatenate when -a is supplied. We seed the option with a base
// value, then append a suffix and assert the resolved value contains
// both halves so the append actually composed instead of replacing.
func TestSetWindowOption_AppendStringListOption(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const session = "swo_append"
	if err := c.CreateSession(ctx, SessionSpec{Name: session, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Seed the per-window override with a base value (no append).
	if err := c.SetWindowOption(ctx, session+":0", "pane-border-format", "BASE",
		false, false, false, false, false); err != nil {
		t.Fatalf("SetWindowOption seed: %v", err)
	}
	// Now append; tmux concatenates the second value onto the first
	// for string-list options like pane-border-format.
	if err := c.SetWindowOption(ctx, session+":0", "pane-border-format", "+EXTRA",
		true, false, false, false, false); err != nil {
		t.Fatalf("SetWindowOption append: %v", err)
	}

	out, err := c.run(ctx, "show-window-options", "-t", session+":0", "pane-border-format")
	if err != nil {
		t.Fatalf("show-window-options: %v", err)
	}
	// The exact tmux quoting of the concatenated value varies (some
	// builds emit `pane-border-format BASE+EXTRA`, others wrap it in
	// quotes); assert on the substrings instead so the test is
	// version-stable.
	if !strings.Contains(out, "BASE") || !strings.Contains(out, "+EXTRA") {
		t.Fatalf("show-window-options output %q does not contain both BASE and +EXTRA after append", out)
	}
}

// TestSetWindowOption_UnsetClearsOverride pins the `-u` flag: after a
// set + unset pair, show-window-options must no longer report a
// per-window override for the option (the line either disappears or
// falls back to the global default). The contract we rely on is that
// the value the override carried (`on`) is no longer the one tmux
// echoes — synchronize-panes defaults to `off`, so a successful unset
// flips the resolved string back.
func TestSetWindowOption_UnsetClearsOverride(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const session = "swo_unset"
	if err := c.CreateSession(ctx, SessionSpec{Name: session, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Set then unset; the second call must succeed and clear the
	// per-window override.
	if err := c.SetWindowOption(ctx, session+":0", "synchronize-panes", "on",
		false, false, false, false, false); err != nil {
		t.Fatalf("SetWindowOption set: %v", err)
	}
	if err := c.SetWindowOption(ctx, session+":0", "synchronize-panes", "",
		false, false, false, false, true); err != nil {
		t.Fatalf("SetWindowOption unset: %v", err)
	}

	// After unset the per-window override is gone; querying without
	// -g returns either an empty body (no override) or the global
	// default, which for synchronize-panes is `off`. Either way, the
	// line "synchronize-panes on" must not be present.
	out, err := c.run(ctx, "show-window-options", "-t", session+":0", "synchronize-panes")
	if err != nil {
		t.Fatalf("show-window-options post-unset: %v", err)
	}
	if strings.Contains(out, "synchronize-panes on") {
		t.Fatalf("expected `synchronize-panes on` to be gone after unset, got %q", out)
	}
}

// TestSetWindowOption_MissingSessionWrapsSentinel pins the typed
// sentinel so the JSON-RPC layer can map "session/window not found"
// to CodeSessionNotFound — the contract every other tmuxctl method
// upholds via run()'s isSessionMissingMsg detector.
func TestSetWindowOption_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise the "server up,
	// session missing" branch (a brand-new socket reports the
	// different "no server running" stderr).
	if err := c.CreateSession(ctx, SessionSpec{Name: "swo_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.SetWindowOption(ctx, "ghost_session_xyzzy:0", "synchronize-panes", "on",
		false, false, false, false, false)
	if err == nil {
		t.Fatal("expected error for missing session, got nil")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSetWindowOption_RejectsEmptyName pins the up-front validation:
// without a name, tmux would otherwise be asked to "set-window-option"
// with no positional and emit a confusing diagnostic. The boundary
// already rejects this via the regex check, but the controller defends
// here too for tests / direct call sites that bypass the JSON-RPC
// layer.
func TestSetWindowOption_RejectsEmptyName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.SetWindowOption(ctx, "demo:0", "", "on",
		false, false, false, false, false)
	if err == nil {
		t.Fatal("expected error for empty name, got nil")
	}
	if !strings.Contains(err.Error(), "option name required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

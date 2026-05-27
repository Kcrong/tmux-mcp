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

// TestBuildSetOptionArgs covers the pure assembly of the argv passed
// to `tmux set-option` for each scope and the unset path. Keeping
// this as a table-driven test (with no live tmux dependency) lets us
// pin every flag/order detail without paying the cost of spinning up
// a controller per row.
func TestBuildSetOptionArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		optName string
		value   string
		scope   string
		target  string
		unset   bool
		want    []string
		wantErr string
	}{
		{
			name:    "server scope ignores target",
			optName: "buffer-limit",
			value:   "100",
			scope:   OptionScopeServer,
			target:  "ignored",
			want:    []string{"set-option", "-s", "buffer-limit", "100"},
		},
		{
			name:    "server scope unset",
			optName: "buffer-limit",
			scope:   OptionScopeServer,
			unset:   true,
			want:    []string{"set-option", "-u", "-s", "buffer-limit"},
		},
		{
			name:    "session scope happy path",
			optName: "status-interval",
			value:   "2",
			scope:   OptionScopeSession,
			target:  "demo",
			want:    []string{"set-option", "-t", "demo", "status-interval", "2"},
		},
		{
			name:    "session scope unset",
			optName: "status-interval",
			scope:   OptionScopeSession,
			target:  "demo",
			unset:   true,
			want:    []string{"set-option", "-u", "-t", "demo", "status-interval"},
		},
		{
			name:    "window scope happy path",
			optName: "automatic-rename",
			value:   "off",
			scope:   OptionScopeWindow,
			target:  "demo:0",
			want:    []string{"set-option", "-w", "-t", "demo:0", "automatic-rename", "off"},
		},
		{
			name:    "window scope unset",
			optName: "automatic-rename",
			scope:   OptionScopeWindow,
			target:  "demo:0",
			unset:   true,
			want:    []string{"set-option", "-u", "-w", "-t", "demo:0", "automatic-rename"},
		},
		{
			name:    "pane scope happy path",
			optName: "remain-on-exit",
			value:   "on",
			scope:   OptionScopePane,
			target:  "demo:0.0",
			want:    []string{"set-option", "-p", "-t", "demo:0.0", "remain-on-exit", "on"},
		},
		{
			name:    "pane scope unset",
			optName: "remain-on-exit",
			scope:   OptionScopePane,
			target:  "%0",
			unset:   true,
			want:    []string{"set-option", "-u", "-p", "-t", "%0", "remain-on-exit"},
		},
		{
			name:    "empty value preserved as positional",
			optName: "command-alias",
			value:   "",
			scope:   OptionScopeServer,
			want:    []string{"set-option", "-s", "command-alias", ""},
		},
		{
			name:    "missing name rejected",
			scope:   OptionScopeServer,
			wantErr: "option name required",
		},
		{
			name:    "session scope without target rejected",
			optName: "status-interval",
			value:   "2",
			scope:   OptionScopeSession,
			wantErr: "target required for scope=session",
		},
		{
			name:    "window scope without target rejected",
			optName: "automatic-rename",
			value:   "off",
			scope:   OptionScopeWindow,
			wantErr: "target required for scope=window",
		},
		{
			name:    "pane scope without target rejected",
			optName: "remain-on-exit",
			value:   "on",
			scope:   OptionScopePane,
			wantErr: "target required for scope=pane",
		},
		{
			name:    "empty scope rejected",
			optName: "something",
			value:   "x",
			wantErr: "scope required",
		},
		{
			name:    "unknown scope rejected",
			optName: "something",
			value:   "x",
			scope:   "everywhere",
			wantErr: "scope must be one of server|session|window|pane",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := buildSetOptionArgs(tc.optName, tc.value, tc.scope, tc.target, tc.unset)
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

// TestSetOption_ValidationErrors exercises the up-front argument
// guards in [Controller.SetOption]. None of these reach tmux, so the
// test is fast and independent of the platform.
func TestSetOption_ValidationErrors(t *testing.T) {
	t.Parallel()
	c := &Controller{} // run() never gets called on the bad-arg path.
	ctx := context.Background()

	cases := []struct {
		name    string
		optName string
		scope   string
		target  string
		wantErr string
	}{
		{name: "missing name", scope: OptionScopeServer, wantErr: "option name required"},
		{name: "empty scope", optName: "anything", wantErr: "scope required"},
		{name: "session scope without target", optName: "status-interval", scope: OptionScopeSession, wantErr: "target required for scope=session"},
		{name: "window scope without target", optName: "automatic-rename", scope: OptionScopeWindow, wantErr: "target required for scope=window"},
		{name: "pane scope without target", optName: "remain-on-exit", scope: OptionScopePane, wantErr: "target required for scope=pane"},
		{name: "unknown scope", optName: "anything", scope: "elsewhere", wantErr: "scope must be one of server|session|window|pane"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.SetOption(ctx, tc.optName, "value", tc.scope, tc.target, false)
			if err == nil {
				t.Fatalf("expected error %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestSetOption_ServerScopeRoundTrip drives the live integration
// path: set a server option, then verify it round-trips through the
// existing ShowOptions helper. buffer-limit is a long-standing tmux
// server option that accepts integer values across every supported
// version.
func TestSetOption_ServerScopeRoundTrip(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session — set-option -s on a
	// brand-new controller (no socket file yet) returns "no server
	// running", which is a real failure mode but not the one we care
	// about for the happy path.
	if err := c.CreateSession(ctx, SessionSpec{Name: "opts_set_srv", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.SetOption(ctx, "buffer-limit", "75", OptionScopeServer, "", false); err != nil {
		t.Fatalf("SetOption(server): %v", err)
	}
	got, err := c.ShowOptions(ctx, OptionScopeServer, "", "", false)
	if err != nil {
		t.Fatalf("ShowOptions(server): %v", err)
	}
	if got["buffer-limit"] != "75" {
		t.Fatalf("buffer-limit = %q, want 75", got["buffer-limit"])
	}
}

// TestSetOption_SessionScopeRoundTrip pins the scope=session happy
// path: set status-interval on a fresh session, then read it back
// through ShowOptions (without -g so we see the override map, not the
// defaults).
func TestSetOption_SessionScopeRoundTrip(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "opts_set_sess"
	if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.SetOption(ctx, "status-interval", "7", OptionScopeSession, name, false); err != nil {
		t.Fatalf("SetOption(session): %v", err)
	}
	got, err := c.ShowOptions(ctx, OptionScopeSession, name, "", false)
	if err != nil {
		t.Fatalf("ShowOptions(session): %v", err)
	}
	if got["status-interval"] != "7" {
		t.Fatalf("status-interval = %q, want 7", got["status-interval"])
	}
}

// TestSetOption_UnsetClearsOverride pins the unset=true path: set an
// override, confirm it is visible, then unset and confirm it is gone
// from the per-session override map. tmux's own contract for
// `set-option -u` is "drop the override; the global default takes
// over", which is what an agent calling unset=true expects.
func TestSetOption_UnsetClearsOverride(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "opts_set_unset"
	if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.SetOption(ctx, "status-interval", "9", OptionScopeSession, name, false); err != nil {
		t.Fatalf("SetOption set: %v", err)
	}
	got, err := c.ShowOptions(ctx, OptionScopeSession, name, "", false)
	if err != nil {
		t.Fatalf("ShowOptions before unset: %v", err)
	}
	if got["status-interval"] != "9" {
		t.Fatalf("status-interval before unset = %q, want 9", got["status-interval"])
	}

	if uerr := c.SetOption(ctx, "status-interval", "", OptionScopeSession, name, true); uerr != nil {
		t.Fatalf("SetOption unset: %v", uerr)
	}
	got, err = c.ShowOptions(ctx, OptionScopeSession, name, "", false)
	if err != nil {
		t.Fatalf("ShowOptions after unset: %v", err)
	}
	if _, present := got["status-interval"]; present {
		t.Fatalf("status-interval still present after unset: %q", got["status-interval"])
	}
}

// TestSetOption_UnknownSessionWrapsSentinel pins the contract relied
// on by the JSON-RPC layer: a target session that does not exist
// surfaces as a wrapped errs.ErrSessionNotFound so the dispatcher can
// map the failure to CodeSessionNotFound (-32000).
func TestSetOption_UnknownSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so we exercise the
	// "server up, named session missing" branch (a fresh controller
	// has no socket file and produces a different error message).
	if err := c.CreateSession(ctx, SessionSpec{Name: "opts_set_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession anchor: %v", err)
	}

	err := c.SetOption(ctx, "status-interval", "1", OptionScopeSession, "definitely_does_not_exist_xyzzy", false)
	if err == nil {
		t.Fatal("expected error setting option on missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

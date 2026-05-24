package tmuxctl

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestBuildShowOptionsArgs covers the pure assembly of the argv passed
// to `tmux show-options` for each scope. Keeping this as a table-driven
// test (with no live tmux dependency) lets us pin every flag/order
// detail without paying the cost of spinning up a controller per row.
func TestBuildShowOptionsArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		scope   string
		session string
		window  string
		global  bool
		want    []string
		wantErr string
	}{
		{
			name:  "server scope ignores session and window",
			scope: OptionScopeServer,
			want:  []string{"show-options", "-s"},
		},
		{
			name:    "server scope drops -g (server opts are already global)",
			scope:   OptionScopeServer,
			session: "ignored",
			window:  "ignored",
			global:  true,
			want:    []string{"show-options", "-s"},
		},
		{
			name:    "session scope without -g",
			scope:   OptionScopeSession,
			session: "demo",
			want:    []string{"show-options", "-t", "demo"},
		},
		{
			name:    "session scope with -g",
			scope:   OptionScopeSession,
			session: "demo",
			global:  true,
			want:    []string{"show-options", "-g", "-t", "demo"},
		},
		{
			name:    "window scope without -g",
			scope:   OptionScopeWindow,
			session: "demo",
			window:  "0",
			want:    []string{"show-options", "-w", "-t", "demo:0"},
		},
		{
			name:    "window scope with -g",
			scope:   OptionScopeWindow,
			session: "demo",
			window:  "main",
			global:  true,
			want:    []string{"show-options", "-w", "-g", "-t", "demo:main"},
		},
		{
			name:    "session scope rejects empty session",
			scope:   OptionScopeSession,
			wantErr: "session required for scope=session",
		},
		{
			name:    "window scope rejects empty session",
			scope:   OptionScopeWindow,
			window:  "0",
			wantErr: "session required for scope=window",
		},
		{
			name:    "window scope rejects empty window",
			scope:   OptionScopeWindow,
			session: "demo",
			wantErr: "window required for scope=window",
		},
		{
			name:    "empty scope rejected",
			wantErr: "scope required",
		},
		{
			name:    "unknown scope rejected",
			scope:   "everything",
			wantErr: "scope must be one of server|session|window",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := buildShowOptionsArgs(tc.scope, tc.session, tc.window, tc.global)
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

// TestParseShowOptions pins the line-by-line parser against the
// formats tmux actually emits in the wild. Scoped to this file so we
// can grow the fixture without coupling to live tmux output.
func TestParseShowOptions(t *testing.T) {
	t.Parallel()
	in := strings.Join([]string{
		"backspace C-?",
		"buffer-limit 50",
		"default-terminal tmux-256color",
		// Values that themselves contain spaces — must come through intact.
		`command-alias[2] "server-info=show-messages -JT"`,
		// Empty single-quoted values are common (history-file '').
		"history-file ''",
		// Trailing CR hardening (in case stdout is read on a system that
		// preserved \r\n line endings).
		"focus-events off\r",
		// A line with no value at all should still be recorded so a
		// caller never silently loses an option name.
		"lonely-key",
		// Blank lines must be skipped without producing an empty key.
		"",
	}, "\n")

	got := parseShowOptions(in)
	want := map[string]string{
		"backspace":        "C-?",
		"buffer-limit":     "50",
		"default-terminal": "tmux-256color",
		"command-alias[2]": `"server-info=show-messages -JT"`,
		"history-file":     "''",
		"focus-events":     "off",
		"lonely-key":       "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed map mismatch\n got: %v\nwant: %v", got, want)
	}
}

// TestShowOptions_ServerScope drives the live integration path: every
// tmux server has a non-empty server-options table from the moment a
// session exists, so this is the load-bearing assertion that the
// scope=server invocation actually round-trips through tmux.
func TestShowOptions_ServerScope(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the tmux server is up. show-options
	// -s on a brand-new controller (no socket file yet) returns
	// "no server running" — that's a genuine error path, not the one
	// we care about for the happy case.
	if err := c.CreateSession(ctx, SessionSpec{Name: "opts_server", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := c.ShowOptions(ctx, OptionScopeServer, "", "", false)
	if err != nil {
		t.Fatalf("ShowOptions(server): %v", err)
	}
	// Every recent tmux build advertises buffer-limit at server scope.
	// We don't pin the exact value because tmux versions disagree, but
	// the key MUST be present — its absence would mean the parser
	// dropped the line entirely.
	if _, ok := got["buffer-limit"]; !ok {
		t.Fatalf("expected buffer-limit in server options, got %v", got)
	}
}

// TestShowOptions_SessionScope_Global pins the scope=session +
// global=true path against a fresh test session. With -g, tmux always
// returns its full session-option defaults, so we have a stable key
// (default-shell) we can assert on without depending on user overrides.
func TestShowOptions_SessionScope_Global(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "opts_session_g"
	if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := c.ShowOptions(ctx, OptionScopeSession, name, "", true)
	if err != nil {
		t.Fatalf("ShowOptions(session, global): %v", err)
	}
	if _, ok := got["default-shell"]; !ok {
		t.Fatalf("expected default-shell among global session options, got %v", got)
	}
}

// TestShowOptions_WindowScope_Global confirms the window scope works
// against a real session+window with the -g flag set so the call
// returns tmux's default window-options table (otherwise the per-window
// override map can be empty on a fresh window).
func TestShowOptions_WindowScope_Global(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "opts_window_g"
	if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := c.ShowOptions(ctx, OptionScopeWindow, name, "0", true)
	if err != nil {
		t.Fatalf("ShowOptions(window, global): %v", err)
	}
	// mode-style is a long-standing tmux window-option default that is
	// safe to assert on across versions.
	if _, ok := got["mode-style"]; !ok {
		t.Fatalf("expected mode-style among global window options, got %v", got)
	}
}

// TestShowOptions_ValidationErrors exercises the up-front argument
// guards in [Controller.ShowOptions]. None of these reach tmux, so the
// test is fast and independent of the platform.
func TestShowOptions_ValidationErrors(t *testing.T) {
	t.Parallel()
	c := &Controller{} // run() never gets called on the bad-arg path.
	ctx := context.Background()

	cases := []struct {
		name    string
		scope   string
		session string
		window  string
		wantErr string
	}{
		{name: "empty scope", wantErr: "scope required"},
		{name: "session scope without session", scope: OptionScopeSession, wantErr: "session required for scope=session"},
		{name: "window scope without session", scope: OptionScopeWindow, window: "0", wantErr: "session required for scope=window"},
		{name: "window scope without window", scope: OptionScopeWindow, session: "demo", wantErr: "window required for scope=window"},
		{name: "unknown scope", scope: "elsewhere", wantErr: "scope must be one of server|session|window"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := c.ShowOptions(ctx, tc.scope, tc.session, tc.window, false)
			if err == nil {
				t.Fatalf("expected error %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

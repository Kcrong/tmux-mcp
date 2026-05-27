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

// TestBuildSetEnvironmentArgs covers the pure assembly of the argv
// passed to `tmux set-environment` for each scope/remove combination.
// Keeping this as a table-driven test (with no live tmux dependency)
// lets us pin every flag/order detail without paying the cost of
// spinning up a controller per row.
func TestBuildSetEnvironmentArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		scope   string
		session string
		envName string
		value   string
		remove  bool
		want    []string
		wantErr string
	}{
		{
			name:    "session scope set with value",
			scope:   EnvironmentScopeSession,
			session: "demo",
			envName: "FOO",
			value:   "bar",
			want:    []string{"set-environment", "-t", "demo", "FOO", "bar"},
		},
		{
			name:    "session scope set with empty value (legal)",
			scope:   EnvironmentScopeSession,
			session: "demo",
			envName: "FOO",
			value:   "",
			want:    []string{"set-environment", "-t", "demo", "FOO", ""},
		},
		{
			name:    "session scope unset",
			scope:   EnvironmentScopeSession,
			session: "demo",
			envName: "FOO",
			remove:  true,
			want:    []string{"set-environment", "-t", "demo", "-u", "FOO"},
		},
		{
			name:    "session scope unset ignores value",
			scope:   EnvironmentScopeSession,
			session: "demo",
			envName: "FOO",
			value:   "ignored",
			remove:  true,
			want:    []string{"set-environment", "-t", "demo", "-u", "FOO"},
		},
		{
			name:    "global scope set",
			scope:   EnvironmentScopeGlobal,
			envName: "PATH",
			value:   "/usr/local/bin",
			want:    []string{"set-environment", "-g", "PATH", "/usr/local/bin"},
		},
		{
			name:    "global scope set ignores session",
			scope:   EnvironmentScopeGlobal,
			session: "ignored",
			envName: "PATH",
			value:   "/usr/local/bin",
			want:    []string{"set-environment", "-g", "PATH", "/usr/local/bin"},
		},
		{
			name:    "global scope unset",
			scope:   EnvironmentScopeGlobal,
			envName: "PATH",
			remove:  true,
			want:    []string{"set-environment", "-g", "-u", "PATH"},
		},
		{
			name:    "session scope rejects empty session",
			scope:   EnvironmentScopeSession,
			envName: "FOO",
			value:   "bar",
			wantErr: "session required for scope=session",
		},
		{
			name:    "empty scope rejected",
			envName: "FOO",
			wantErr: "scope required",
		},
		{
			name:    "unknown scope rejected",
			scope:   "everywhere",
			envName: "FOO",
			wantErr: "scope must be one of session|global",
		},
		{
			name:    "empty name rejected",
			scope:   EnvironmentScopeGlobal,
			wantErr: "name required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := buildSetEnvironmentArgs(tc.scope, tc.session, tc.envName, tc.value, tc.remove)
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

// TestSetEnvironment_SessionRoundTrip drives the live integration path:
// set a per-session variable, then probe tmux's own
// `show-environment -t SESSION NAME` to confirm the value landed where
// future panes will see it. Pinned to scope=session because that is the
// default the JSON-RPC boundary surfaces.
func TestSetEnvironment_SessionRoundTrip(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const session = "env_session_rt"
	if err := c.CreateSession(ctx, SessionSpec{Name: session, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.SetEnvironment(ctx, EnvironmentScopeSession, session, "MCP_FOO", "bar", false); err != nil {
		t.Fatalf("SetEnvironment(set): %v", err)
	}
	out, err := c.run(ctx, "show-environment", "-t", session, "MCP_FOO")
	if err != nil {
		t.Fatalf("show-environment after set: %v", err)
	}
	got := strings.TrimSpace(out)
	if got != "MCP_FOO=bar" {
		t.Fatalf("show-environment = %q, want %q", got, "MCP_FOO=bar")
	}

	// Now remove and confirm tmux reports the variable as no-longer-set.
	// tmux 3.4 drops the session-level entry on `-u` and a follow-up
	// `show-environment -t SESSION NAME` exits non-zero with
	// "unknown variable" — the typed sentinel we want to inherit
	// regardless of which tmux version is on PATH.
	if err := c.SetEnvironment(ctx, EnvironmentScopeSession, session, "MCP_FOO", "", true); err != nil {
		t.Fatalf("SetEnvironment(remove): %v", err)
	}
	if _, runErr := c.run(ctx, "show-environment", "-t", session, "MCP_FOO"); runErr == nil {
		t.Fatalf("show-environment after remove succeeded; want error")
	}
}

// TestSetEnvironment_GlobalRoundTrip pins the scope=global path. Global
// entries are visible via `show-environment -g NAME` regardless of any
// session, so we only need the controller's tmux server to be running
// (anchored via a throwaway session).
func TestSetEnvironment_GlobalRoundTrip(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "env_global_rt_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.SetEnvironment(ctx, EnvironmentScopeGlobal, "", "MCP_GLOBAL", "yes", false); err != nil {
		t.Fatalf("SetEnvironment(set, global): %v", err)
	}
	out, err := c.run(ctx, "show-environment", "-g", "MCP_GLOBAL")
	if err != nil {
		t.Fatalf("show-environment -g after set: %v", err)
	}
	got := strings.TrimSpace(out)
	if got != "MCP_GLOBAL=yes" {
		t.Fatalf("show-environment -g = %q, want %q", got, "MCP_GLOBAL=yes")
	}

	if err := c.SetEnvironment(ctx, EnvironmentScopeGlobal, "", "MCP_GLOBAL", "", true); err != nil {
		t.Fatalf("SetEnvironment(remove, global): %v", err)
	}
	// After a global -u, tmux drops the variable entirely, so a
	// follow-up `show-environment -g NAME` exits non-zero with
	// "unknown variable". Surface the error rather than asserting on
	// stdout — that would race with tmux versions that emit a header.
	if _, runErr := c.run(ctx, "show-environment", "-g", "MCP_GLOBAL"); runErr == nil {
		t.Fatalf("show-environment -g MCP_GLOBAL succeeded after remove; want error")
	}
}

// TestSetEnvironment_UnknownSessionTyped pins the wire contract for
// "session does not exist": the controller must wrap
// errs.ErrSessionNotFound so the JSON-RPC dispatcher maps it onto
// CodeSessionNotFound (-32000).
func TestSetEnvironment_UnknownSessionTyped(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so the tmux server is up; without it, the
	// "no server running" branch would mask the session-not-found
	// detection we want to assert on.
	if err := c.CreateSession(ctx, SessionSpec{Name: "env_anchor_unknown", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.SetEnvironment(ctx, EnvironmentScopeSession, "definitely_not_a_session", "FOO", "bar", false)
	if err == nil {
		t.Fatalf("SetEnvironment against missing session: want error, got nil")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("SetEnvironment err = %v, want errs.ErrSessionNotFound", err)
	}
}

// TestSetEnvironment_ValidationErrors exercises the up-front argument
// guards in [Controller.SetEnvironment]. None of these reach tmux, so
// the test is fast and independent of the platform.
func TestSetEnvironment_ValidationErrors(t *testing.T) {
	t.Parallel()
	c := &Controller{} // run() never gets called on the bad-arg path.
	ctx := context.Background()

	cases := []struct {
		name    string
		scope   string
		session string
		envName string
		value   string
		remove  bool
		wantErr string
	}{
		{name: "empty name", scope: EnvironmentScopeGlobal, wantErr: "name required"},
		{name: "empty scope", envName: "FOO", wantErr: "scope required"},
		{name: "session scope without session", scope: EnvironmentScopeSession, envName: "FOO", wantErr: "session required for scope=session"},
		{name: "unknown scope", scope: "elsewhere", envName: "FOO", wantErr: "scope must be one of session|global"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.SetEnvironment(ctx, tc.scope, tc.session, tc.envName, tc.value, tc.remove)
			if err == nil {
				t.Fatalf("expected error %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

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

// TestBuildShowEnvironmentArgs covers the pure assembly of the argv
// passed to `tmux show-environment` for each scope/name combination.
// Keeping this as a table-driven test (with no live tmux dependency)
// pins every flag/order detail without paying the cost of spinning up
// a controller per row.
func TestBuildShowEnvironmentArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		envName string
		scope   string
		target  string
		want    []string
		wantErr string
	}{
		{
			name:   "session scope all vars",
			scope:  EnvironmentScopeSession,
			target: "demo",
			want:   []string{"show-environment", "-t", "demo"},
		},
		{
			name:    "session scope single var",
			envName: "FOO",
			scope:   EnvironmentScopeSession,
			target:  "demo",
			want:    []string{"show-environment", "-t", "demo", "FOO"},
		},
		{
			name:  "global scope all vars",
			scope: EnvironmentScopeGlobal,
			want:  []string{"show-environment", "-g"},
		},
		{
			name:    "global scope single var",
			envName: "PATH",
			scope:   EnvironmentScopeGlobal,
			want:    []string{"show-environment", "-g", "PATH"},
		},
		{
			name:   "global scope ignores target",
			scope:  EnvironmentScopeGlobal,
			target: "ignored",
			want:   []string{"show-environment", "-g"},
		},
		{
			name:    "session scope rejects empty target",
			scope:   EnvironmentScopeSession,
			wantErr: "target required for scope=session",
		},
		{
			name:    "empty scope rejected",
			wantErr: "scope required",
		},
		{
			name:    "unknown scope rejected",
			scope:   "everywhere",
			wantErr: "scope must be one of session|global",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := buildShowEnvironmentArgs(tc.envName, tc.scope, tc.target)
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

// TestParseShowEnvironment covers the line-level parser end of
// [Controller.ShowEnvironment]. tmux emits one of two shapes per
// line — `NAME=VALUE` for present entries, `-NAME` for entries
// explicitly removed on top of a global default — and the parser
// must round-trip both into [EnvEntry] with the right Present flag.
func TestParseShowEnvironment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want map[string]EnvEntry
	}{
		{
			name: "empty body",
			in:   "",
			want: map[string]EnvEntry{},
		},
		{
			name: "single present",
			in:   "FOO=bar\n",
			want: map[string]EnvEntry{
				"FOO": {Name: "FOO", Value: "bar", Present: true},
			},
		},
		{
			name: "single removed",
			in:   "-FOO\n",
			want: map[string]EnvEntry{
				"FOO": {Name: "FOO", Present: false},
			},
		},
		{
			name: "value with empty string",
			in:   "EMPTY=\n",
			want: map[string]EnvEntry{
				"EMPTY": {Name: "EMPTY", Value: "", Present: true},
			},
		},
		{
			name: "value containing equals signs",
			in:   "KEY=foo=bar=baz\n",
			want: map[string]EnvEntry{
				"KEY": {Name: "KEY", Value: "foo=bar=baz", Present: true},
			},
		},
		{
			name: "value with spaces",
			in:   "FANCY=hello world\n",
			want: map[string]EnvEntry{
				"FANCY": {Name: "FANCY", Value: "hello world", Present: true},
			},
		},
		{
			name: "mixed present and removed",
			in:   "FOO=bar\n-BAZ\nQUX=\n",
			want: map[string]EnvEntry{
				"FOO": {Name: "FOO", Value: "bar", Present: true},
				"BAZ": {Name: "BAZ", Present: false},
				"QUX": {Name: "QUX", Value: "", Present: true},
			},
		},
		{
			name: "blank lines skipped",
			in:   "FOO=bar\n\n\nBAZ=qux\n",
			want: map[string]EnvEntry{
				"FOO": {Name: "FOO", Value: "bar", Present: true},
				"BAZ": {Name: "BAZ", Value: "qux", Present: true},
			},
		},
		{
			name: "carriage returns stripped",
			in:   "FOO=bar\r\nBAZ=qux\r\n",
			want: map[string]EnvEntry{
				"FOO": {Name: "FOO", Value: "bar", Present: true},
				"BAZ": {Name: "BAZ", Value: "qux", Present: true},
			},
		},
		{
			name: "lone dash dropped",
			in:   "-\n",
			want: map[string]EnvEntry{},
		},
		{
			name: "lone equals dropped",
			in:   "=junk\n",
			want: map[string]EnvEntry{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseShowEnvironment(tc.in)
			if !reflect.DeepEqual(got.Vars, tc.want) {
				t.Fatalf("vars mismatch\n got: %v\nwant: %v", got.Vars, tc.want)
			}
		})
	}
}

// TestShowEnvironment_RoundTripSession is the load-bearing
// integration test: set FOO=bar via SetEnvironment, then
// ShowEnvironment must report the same value (whole-table form)
// and report present=true for the single-name probe form. We
// fall back to driving tmux directly when SetEnvironment has not
// landed yet (PR #111) so this PR does not have to wait on its
// merge.
func TestShowEnvironment_RoundTripSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const session = "show_env_rt"
	if err := c.CreateSession(ctx, SessionSpec{Name: session, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Seed via raw run() rather than SetEnvironment so this test
	// stands alone of PR #111. Once SetEnvironment lands and is
	// part of the same package, callers can swap the seed line for
	// `c.SetEnvironment(ctx, EnvironmentScopeSession, session, "MCP_FOO", "bar", false)`
	// without changing the rest of the test.
	if _, err := c.run(ctx, "set-environment", "-t", session, "MCP_FOO", "bar"); err != nil {
		t.Fatalf("seed FOO=bar: %v", err)
	}

	dump, err := c.ShowEnvironment(ctx, "", EnvironmentScopeSession, session)
	if err != nil {
		t.Fatalf("ShowEnvironment(all): %v", err)
	}
	got, ok := dump.Vars["MCP_FOO"]
	if !ok {
		t.Fatalf("MCP_FOO missing from dump: %v", dump.Vars)
	}
	if got.Value != "bar" || !got.Present {
		t.Fatalf("MCP_FOO = %+v, want {Value:bar Present:true}", got)
	}

	// Single-name probe must echo back the same entry.
	single, err := c.ShowEnvironment(ctx, "MCP_FOO", EnvironmentScopeSession, session)
	if err != nil {
		t.Fatalf("ShowEnvironment(single): %v", err)
	}
	if len(single.Vars) != 1 {
		t.Fatalf("single-name dump has %d entries, want 1: %v", len(single.Vars), single.Vars)
	}
	got = single.Vars["MCP_FOO"]
	if got.Value != "bar" || !got.Present {
		t.Fatalf("single MCP_FOO = %+v, want {Value:bar Present:true}", got)
	}
}

// TestShowEnvironment_RemovedEntryPresentFalse drives the
// tmux-marks-it-removed path: a session-scope `-r` (or, equivalently,
// a `-u NAME` that hides an inherited global) emits `-NAME` and the
// parser must surface that as Present=false with no value.
//
// We seed via direct run() calls so this test does not depend on
// PR #111's SetEnvironment landing first.
func TestShowEnvironment_RemovedEntryPresentFalse(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const session = "show_env_removed"
	if err := c.CreateSession(ctx, SessionSpec{Name: session, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Seed a global, then explicitly mark it "removed" at session
	// scope via tmux's `-r` flag — the only way to get the leading
	// `-NAME` line without resorting to PR #111's SetEnvironment.
	if _, err := c.run(ctx, "set-environment", "-g", "MCP_REMOVED", "yes"); err != nil {
		t.Fatalf("seed global MCP_REMOVED: %v", err)
	}
	if _, err := c.run(ctx, "set-environment", "-t", session, "-r", "MCP_REMOVED"); err != nil {
		t.Fatalf("session-scope -r MCP_REMOVED: %v", err)
	}

	dump, err := c.ShowEnvironment(ctx, "MCP_REMOVED", EnvironmentScopeSession, session)
	if err != nil {
		t.Fatalf("ShowEnvironment(MCP_REMOVED): %v", err)
	}
	got, ok := dump.Vars["MCP_REMOVED"]
	if !ok {
		t.Fatalf("MCP_REMOVED missing from dump: %v", dump.Vars)
	}
	if got.Present {
		t.Errorf("Present = true, want false (tmux marked the entry removed)")
	}
	if got.Value != "" {
		t.Errorf("Value = %q, want empty (removed entries carry no value)", got.Value)
	}
}

// TestShowEnvironment_GlobalScope pins the scope=global path. Global
// entries are visible via `show-environment -g NAME` regardless of
// any session, so we only need the controller's tmux server to be
// running (anchored via a throwaway session).
func TestShowEnvironment_GlobalScope(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "show_env_global_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.run(ctx, "set-environment", "-g", "MCP_GLOBAL_PROBE", "ok"); err != nil {
		t.Fatalf("seed -g MCP_GLOBAL_PROBE: %v", err)
	}

	dump, err := c.ShowEnvironment(ctx, "MCP_GLOBAL_PROBE", EnvironmentScopeGlobal, "")
	if err != nil {
		t.Fatalf("ShowEnvironment(global): %v", err)
	}
	got, ok := dump.Vars["MCP_GLOBAL_PROBE"]
	if !ok {
		t.Fatalf("MCP_GLOBAL_PROBE missing: %v", dump.Vars)
	}
	if got.Value != "ok" || !got.Present {
		t.Fatalf("MCP_GLOBAL_PROBE = %+v, want {Value:ok Present:true}", got)
	}
}

// TestShowEnvironment_UnknownVariableSentinel pins the wire contract
// for "the variable was never set in this scope": tmux exits non-zero
// with `unknown variable: NAME`, and the controller must wrap that
// as [ErrEnvNameNotSet] so the boundary can branch on the typed
// sentinel instead of substring-matching tmux's stderr.
func TestShowEnvironment_UnknownVariableSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const session = "show_env_unknown"
	if err := c.CreateSession(ctx, SessionSpec{Name: session, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err := c.ShowEnvironment(ctx, "MCP_NEVER_SET", EnvironmentScopeSession, session)
	if err == nil {
		t.Fatal("ShowEnvironment(MCP_NEVER_SET): want error, got nil")
	}
	if !errors.Is(err, ErrEnvNameNotSet) {
		t.Fatalf("error %v does not wrap ErrEnvNameNotSet", err)
	}
}

// TestShowEnvironment_UnknownSessionTyped pins the wire contract for
// "session does not exist": the controller must wrap
// errs.ErrSessionNotFound so the JSON-RPC dispatcher maps it onto
// CodeSessionNotFound (-32000).
func TestShowEnvironment_UnknownSessionTyped(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so the tmux server is up; without it,
	// the "no server running" branch would mask the
	// session-not-found detection we want to assert on.
	if err := c.CreateSession(ctx, SessionSpec{Name: "show_env_anchor_unknown", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err := c.ShowEnvironment(ctx, "", EnvironmentScopeSession, "definitely_not_a_session")
	if err == nil {
		t.Fatal("ShowEnvironment against missing session: want error, got nil")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestShowEnvironment_ValidationErrors exercises the up-front
// argument guards in [Controller.ShowEnvironment]. None of these
// reach tmux, so the test is fast and independent of the platform.
func TestShowEnvironment_ValidationErrors(t *testing.T) {
	t.Parallel()
	c := &Controller{} // run() never gets called on the bad-arg path.
	ctx := context.Background()

	cases := []struct {
		name    string
		envName string
		scope   string
		target  string
		wantErr string
	}{
		{name: "empty scope", wantErr: "scope required"},
		{name: "session scope without target", scope: EnvironmentScopeSession, wantErr: "target required for scope=session"},
		{name: "unknown scope", scope: "elsewhere", wantErr: "scope must be one of session|global"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := c.ShowEnvironment(ctx, tc.envName, tc.scope, tc.target)
			if err == nil {
				t.Fatalf("expected error %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

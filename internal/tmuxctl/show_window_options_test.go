package tmuxctl

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestBuildShowWindowOptionsArgs covers the pure assembly of argv passed
// to `tmux show-window-options`. Keeping it as a table-driven test (no
// live tmux dependency) lets us pin every flag/order detail without
// paying the cost of spinning up a controller per row. Mirrors
// TestBuildShowOptionsArgs's structure so a future contributor reading
// either file sees a consistent shape.
func TestBuildShowWindowOptionsArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target string
		opt    string
		global bool
		want   []string
	}{
		{
			name: "no flags emits bare verb",
			want: []string{"show-window-options"},
		},
		{
			name:   "target appends -t",
			target: "demo:0",
			want:   []string{"show-window-options", "-t", "demo:0"},
		},
		{
			name:   "global prepends -g before -t",
			target: "demo:0",
			global: true,
			want:   []string{"show-window-options", "-g", "-t", "demo:0"},
		},
		{
			name:   "name positional comes after -t",
			target: "demo:0",
			opt:    "synchronize-panes",
			want:   []string{"show-window-options", "-t", "demo:0", "synchronize-panes"},
		},
		{
			name:   "global without target keeps -g",
			global: true,
			want:   []string{"show-window-options", "-g"},
		},
		{
			name:   "global+target+name preserves canonical order",
			target: "demo:main",
			opt:    "mode-keys",
			global: true,
			want:   []string{"show-window-options", "-g", "-t", "demo:main", "mode-keys"},
		},
		{
			name: "name without target still trails as positional",
			opt:  "synchronize-panes",
			want: []string{"show-window-options", "synchronize-panes"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildShowWindowOptionsArgs(tc.target, tc.opt, tc.global)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("args mismatch\n got: %v\nwant: %v", got, tc.want)
			}
		})
	}
}

// TestIsWindowMissingMsg pins the substring detector that maps tmux's
// version-dependent phrasing onto the typed [errs.ErrSessionNotFound]
// sentinel. Comparing the lowercased message keeps the check robust
// against tmux's case quirks across versions.
func TestIsWindowMissingMsg(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		msg  string
		want bool
	}{
		{name: "lowercase phrase", msg: "no such window: demo:0", want: true},
		{name: "embedded in command echo", msg: "tmux show-window-options -t demo: no such window: demo:", want: true},
		{name: "mixed case", msg: "No Such Window: demo:0", want: true},
		{name: "unrelated tmux error", msg: "ambiguous option: mode", want: false},
		{name: "empty", msg: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isWindowMissingMsg(tc.msg); got != tc.want {
				t.Fatalf("isWindowMissingMsg(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

// TestShowWindowOptions_GlobalDefaults drives the live integration path
// for the `-g` view. Every tmux build advertises a long, non-empty
// global window-options table from the moment a session exists, so this
// is the load-bearing assertion that the round-trip through tmux
// actually works. mode-keys is a long-standing key we can pin without
// depending on a specific tmux build.
func TestShowWindowOptions_GlobalDefaults(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the tmux server is up. Without one,
	// `show-window-options` would fail with "no server running" — that's
	// a separate error path we cover elsewhere.
	const name = "swo_global"
	if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := c.ShowWindowOptions(ctx, name+":0", "", true)
	if err != nil {
		t.Fatalf("ShowWindowOptions(global): %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected non-empty global window options, got empty slice")
	}
	if !containsKey(got, "mode-keys") {
		t.Fatalf("expected mode-keys among global window options, got %v", names(got))
	}
}

// TestShowWindowOptions_ByName_AfterSet pins the load-bearing
// integration path: set a per-window override via raw set-window-option,
// then ShowWindowOptions(target, name, false) returns exactly one entry
// with the value tmux echoes back. synchronize-panes is the canonical
// per-window flag MCP agents care about, so pinning it here doubles as a
// regression guard for the agent-introspection use case.
func TestShowWindowOptions_ByName_AfterSet(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "swo_byname"
	if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Drive the tmux state the test wants to introspect via the raw
	// set-window-option command — the brief explicitly calls for this
	// shape so the test exercises the read path against state set by
	// tmux directly (not by an as-yet-unmerged set_window_option tool).
	if _, err := c.run(ctx, "set-window-option", "-t", name+":0", "synchronize-panes", "on"); err != nil {
		t.Fatalf("set-window-option: %v", err)
	}

	got, err := c.ShowWindowOptions(ctx, name+":0", "synchronize-panes", false)
	if err != nil {
		t.Fatalf("ShowWindowOptions(by-name): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("by-name query should return exactly one entry, got %d (%v)", len(got), got)
	}
	if got[0].Name != "synchronize-panes" {
		t.Fatalf("entry name = %q, want synchronize-panes", got[0].Name)
	}
	if got[0].Value != "on" {
		t.Fatalf("synchronize-panes = %q, want %q", got[0].Value, "on")
	}
}

// TestShowWindowOptions_AllOverrides_AfterSet covers the long-list path:
// set a per-window override and then read every override on the target
// (no name positional, no -g). The resulting slice must contain the
// override we just set, with the right value, and may or may not contain
// other entries depending on tmux's defaults; either way the slice MUST
// be non-empty.
func TestShowWindowOptions_AllOverrides_AfterSet(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "swo_all"
	if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.run(ctx, "set-window-option", "-t", name+":0", "synchronize-panes", "on"); err != nil {
		t.Fatalf("set-window-option: %v", err)
	}

	got, err := c.ShowWindowOptions(ctx, name+":0", "", false)
	if err != nil {
		t.Fatalf("ShowWindowOptions(all): %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected at least the override we just set, got empty slice")
	}
	found := false
	for _, e := range got {
		if e.Name == "synchronize-panes" {
			found = true
			if e.Value != "on" {
				t.Fatalf("synchronize-panes = %q, want %q", e.Value, "on")
			}
		}
	}
	if !found {
		t.Fatalf("synchronize-panes missing from override listing %v", names(got))
	}
}

// TestShowWindowOptions_EmptyResult pins the "no overrides set, no -g"
// branch: a fresh window has no per-window overrides, so the call must
// return an empty slice (not an error). This is the contract the
// boundary tool relies on so a well-formed query that simply has nothing
// to report does not surface as a typed error.
func TestShowWindowOptions_EmptyResult(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "swo_empty"
	if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := c.ShowWindowOptions(ctx, name+":0", "", false)
	if err != nil {
		t.Fatalf("ShowWindowOptions(empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("fresh window should have no overrides, got %v", got)
	}
}

// TestShowWindowOptions_ByName_UnsetReturnsEmpty pins the contract that
// asking for a single option that is not currently set on the target
// returns an empty slice (tmux prints no output in that case). Without
// this, callers querying a sparse override map would see a confusing
// "internal error" instead of a clean "no value".
func TestShowWindowOptions_ByName_UnsetReturnsEmpty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const name = "swo_unset"
	if err := c.CreateSession(ctx, SessionSpec{Name: name, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := c.ShowWindowOptions(ctx, name+":0", "synchronize-panes", false)
	if err != nil {
		t.Fatalf("ShowWindowOptions(by-name unset): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unset by-name query should return empty slice, got %v", got)
	}
}

// TestShowWindowOptions_MissingSessionWrapsSentinel proves the controller
// surfaces an unknown session/window via the typed
// [errs.ErrSessionNotFound] sentinel — relied on by the dispatcher to
// emit CodeSessionNotFound on the wire. tmux 3.4 emits "no such window:
// <target>" for this branch, which is not covered by the broader
// run()-level isSessionMissingMsg check; the wrapping in
// ShowWindowOptions itself is what closes the gap.
func TestShowWindowOptions_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor so the tmux server is definitely up; without it tmux would
	// fail with "no server running" — a different branch we are not
	// asserting on here.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err := c.ShowWindowOptions(ctx, "definitely_does_not_exist_xyzzy:0", "", false)
	if err == nil {
		t.Fatal("expected error for missing session/window")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// containsKey reports whether the slice carries an OptionEntry with the
// given name. Sugar for a few of the assertions above so the failure
// message can name the missing key without rebuilding a map at every
// call site.
func containsKey(entries []OptionEntry, key string) bool {
	for _, e := range entries {
		if e.Name == key {
			return true
		}
	}
	return false
}

// names extracts the Name field of every entry into a []string for
// readable failure messages — printing the full []OptionEntry would
// dump every value across the table and obscure the missing key.
func names(entries []OptionEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

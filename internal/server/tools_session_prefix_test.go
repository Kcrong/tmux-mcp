package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestSessionPrefix_EmptyBackCompat pins the historical zero-config
// behaviour: with [Tools.SessionPrefix] left at its zero value, the
// dispatch path must not prefix or filter anything. Sessions land on
// tmux under their literal names, session_list returns those names
// verbatim, and a follow-up tool call ("capture") addresses them by
// the same name. This is the contract every existing deployment
// depends on — the prefix feature is opt-in.
func TestSessionPrefix_EmptyBackCompat(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	if tools.SessionPrefix != "" {
		t.Fatalf("default SessionPrefix = %q, want empty", tools.SessionPrefix)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	call := func(name string, args any) any {
		t.Helper()
		params := mustJSON(t, map[string]any{"name": name, "arguments": args})
		res, rerr := tools.Handle(ctx, "tools/call", params)
		if rerr != nil {
			t.Fatalf("%s: %s", name, rerr.Message)
		}
		return res
	}

	const want = "spx_back_compat"
	call("session_create", map[string]any{
		"name": want, "command": "/bin/sh", "width": 80, "height": 20,
	})
	// Use the controller directly with a fresh background context for
	// the cleanup so the kill survives the test's own ctx cancel
	// (otherwise the t.Cleanup race trips "context canceled" on the
	// kill-session command). The controller path is what session_kill
	// would have called anyway.
	t.Cleanup(func() {
		_ = tools.Ctl.KillSession(context.Background(), want)
		tools.Snap.Forget(want)
	})

	// session_list must echo the literal name (no prefix in either
	// direction) since the feature is disabled.
	listRes := extractText(t, call("session_list", map[string]any{}))
	var listPayload struct {
		Sessions []string `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(listRes), &listPayload); err != nil {
		t.Fatalf("decode session_list: %v\nbody=%s", err, listRes)
	}
	found := false
	for _, n := range listPayload.Sessions {
		if n == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected %q in session_list, got %v", want, listPayload.Sessions)
	}

	// The bare name must still address the session for follow-up tools.
	_ = extractText(t, call("capture", map[string]any{"session": want}))
}

// TestSessionPrefix_CreateMapsToPrefixedTmuxName verifies the
// transparent-rewrite contract on session_create: the bare name the
// client supplied lands on tmux as "<prefix><name>", but the response
// echoes the bare name back so the caller never sees the prefixed
// form. tmux's view is asserted via [Controller.ListSessions] (which
// returns raw tmux names), and session_list is asserted to perform
// the inverse strip — both sides confirm the abstraction holds.
func TestSessionPrefix_CreateMapsToPrefixedTmuxName(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	tools.SessionPrefix = "spx_one_"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	call := func(name string, args any) any {
		t.Helper()
		params := mustJSON(t, map[string]any{"name": name, "arguments": args})
		res, rerr := tools.Handle(ctx, "tools/call", params)
		if rerr != nil {
			t.Fatalf("%s: %s", name, rerr.Message)
		}
		return res
	}

	const logical = "demo"
	const wantTmuxName = "spx_one_demo"
	resText := extractText(t, call("session_create", map[string]any{
		"name": logical, "command": "/bin/sh", "width": 80, "height": 20,
	}))
	if !strings.Contains(resText, `"`+logical+`"`) {
		t.Fatalf("expected response to echo logical name %q, got %q", logical, resText)
	}
	if strings.Contains(resText, wantTmuxName) {
		t.Fatalf("response leaked prefixed tmux name %q: %q", wantTmuxName, resText)
	}
	t.Cleanup(func() {
		// Kill via the tmux-real name so cleanup runs even if the
		// SessionPrefix is later mutated by another sub-test.
		_ = tools.Ctl.KillSession(context.Background(), wantTmuxName)
		tools.Snap.Forget(wantTmuxName)
	})

	// Controller view: tmux must hold the prefixed name.
	rawNames, err := tools.Ctl.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ctl.ListSessions: %v", err)
	}
	gotPrefixed := false
	for _, n := range rawNames {
		if n == wantTmuxName {
			gotPrefixed = true
			break
		}
		if n == logical {
			t.Fatalf("found bare logical name %q on tmux; expected only %q", logical, wantTmuxName)
		}
	}
	if !gotPrefixed {
		t.Fatalf("tmux view missing %q; raw list = %v", wantTmuxName, rawNames)
	}

	// session_list view: client must see the logical name with no
	// prefix on the wire.
	listRes := extractText(t, call("session_list", map[string]any{}))
	var listPayload struct {
		Sessions []string `json:"sessions"`
	}
	if jerr := json.Unmarshal([]byte(listRes), &listPayload); jerr != nil {
		t.Fatalf("decode session_list: %v\nbody=%s", jerr, listRes)
	}
	if len(listPayload.Sessions) != 1 {
		t.Fatalf("session_list = %v, want exactly [%q]", listPayload.Sessions, logical)
	}
	if listPayload.Sessions[0] != logical {
		t.Fatalf("session_list[0] = %q, want %q (prefix should be stripped)",
			listPayload.Sessions[0], logical)
	}

	// session_kill addressed by the bare logical name must hit the
	// prefixed session on tmux.
	_ = extractText(t, call("session_kill", map[string]any{"name": logical}))
	rawAfter, err := tools.Ctl.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ctl.ListSessions after kill: %v", err)
	}
	for _, n := range rawAfter {
		if n == wantTmuxName {
			t.Fatalf("session_kill(%q) failed to remove %q from tmux", logical, wantTmuxName)
		}
	}
}

// TestSessionPrefix_CrossTenantIsolation verifies the multi-agent
// scenario the flag exists for: two *Tools instances pointed at the
// same controller but with different prefixes must each see only
// their own sessions in session_list, and kill_all_sessions on one
// must leave the other's sessions running. A "neutral" un-prefixed
// session created via the controller directly is also invisible to
// the prefixed instances — that is the contract that lets a
// shared tmux server host multiple agents safely.
func TestSessionPrefix_CrossTenantIsolation(t *testing.T) {
	skipIfNoTmux(t)
	// Both *Tools instances share the controller from the first one so
	// they really do drive the same tmux server (the multi-agent setup
	// the flag is built for).
	alice := newTools(t)
	alice.SessionPrefix = "ag_alice_"
	bob := NewTools(alice.Ctl)
	bob.SessionPrefix = "ag_bob_"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	mkCall := func(tools *Tools) func(name string, args any) any {
		return func(name string, args any) any {
			t.Helper()
			params := mustJSON(t, map[string]any{"name": name, "arguments": args})
			res, rerr := tools.Handle(ctx, "tools/call", params)
			if rerr != nil {
				t.Fatalf("%s: %s", name, rerr.Message)
			}
			return res
		}
	}
	callAlice := mkCall(alice)
	callBob := mkCall(bob)

	callAlice("session_create", map[string]any{
		"name": "build", "command": "/bin/sh", "width": 80, "height": 20,
	})
	callBob("session_create", map[string]any{
		"name": "build", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		// Best-effort: forget then kill so a double-cleanup never panics.
		_ = alice.Ctl.KillSession(context.Background(), "ag_alice_build")
		_ = alice.Ctl.KillSession(context.Background(), "ag_bob_build")
	})

	// Tmux view: both prefixed sessions must coexist.
	raw, err := alice.Ctl.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ctl.ListSessions: %v", err)
	}
	gotAlice, gotBob := false, false
	for _, n := range raw {
		if n == "ag_alice_build" {
			gotAlice = true
		}
		if n == "ag_bob_build" {
			gotBob = true
		}
	}
	if !gotAlice || !gotBob {
		t.Fatalf("expected both ag_alice_build and ag_bob_build on tmux, got %v", raw)
	}

	// Each instance must see only its own session in session_list.
	for _, tc := range []struct {
		who   string
		fn    func(name string, args any) any
		want  string
		other string
	}{
		{"alice", callAlice, "build", "ag_bob_build"},
		{"bob", callBob, "build", "ag_alice_build"},
	} {
		listRes := extractText(t, tc.fn("session_list", map[string]any{}))
		var lp struct {
			Sessions []string `json:"sessions"`
		}
		if jerr := json.Unmarshal([]byte(listRes), &lp); jerr != nil {
			t.Fatalf("%s: decode session_list: %v\nbody=%s", tc.who, jerr, listRes)
		}
		if len(lp.Sessions) != 1 || lp.Sessions[0] != tc.want {
			t.Fatalf("%s: session_list = %v, want exactly [%q]", tc.who, lp.Sessions, tc.want)
		}
		// The cross-tenant raw name must never appear in either form.
		for _, s := range lp.Sessions {
			if s == tc.other || strings.Contains(s, "ag_") {
				t.Fatalf("%s: cross-tenant leak %q in %v", tc.who, s, lp.Sessions)
			}
		}
	}

	// kill_all_sessions on alice must leave bob's session running.
	_ = extractText(t, callAlice("kill_all_sessions", map[string]any{}))
	rawAfter, err := alice.Ctl.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ctl.ListSessions after alice kill_all: %v", err)
	}
	bobStillUp := false
	for _, n := range rawAfter {
		if n == "ag_alice_build" {
			t.Fatalf("alice kill_all left her own session up: %v", rawAfter)
		}
		if n == "ag_bob_build" {
			bobStillUp = true
		}
	}
	if !bobStillUp {
		t.Fatalf("alice kill_all reaped bob's session; raw view = %v", rawAfter)
	}
}

// TestSessionPrefix_RuntimeRejectsCombinedOverflow guards the per-call
// length budget enforced by [validateCombinedSessionName]: even when
// the prefix and the user-supplied name each pass their individual
// regex/length checks, their concatenation must not overflow tmux's
// 64-byte session-name ceiling. A handler-level invalidParams is the
// expected response — without it an oversized combination would land
// on tmux as a name no other tool can validly reference.
func TestSessionPrefix_RuntimeRejectsCombinedOverflow(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	tools.SessionPrefix = strings.Repeat("p", 60) + "_" // len 61, leaves 3 bytes
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	overflow := strings.Repeat("x", 10) // 61 + 10 > 64
	params := mustJSON(t, map[string]any{
		"name": "session_create",
		"arguments": map[string]any{
			"name": overflow, "command": "/bin/sh", "width": 80, "height": 20,
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected invalidParams for overflow, got result %v", res)
	}
	if rerr.Code != codeInvalidParams {
		t.Fatalf("err code = %d, want %d (invalidParams)", rerr.Code, codeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "exceeds") {
		t.Fatalf("err message %q must explain the overflow", rerr.Message)
	}
}

// TestValidateSessionPrefix exercises the startup-time validator that
// [main] runs against the operator-supplied -session-prefix flag. The
// table covers every documented rule: empty is OK, the regex must
// match, no trailing dash, and the prefix must leave room for at
// least one byte of session name (so a prefix length of
// maxSessionNameLen-1 or shorter is the upper bound).
func TestValidateSessionPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prefix  string
		wantErr bool
	}{
		{"empty-ok", "", false},
		{"single-char-ok", "a", false},
		{"underscore-suffix-ok", "agent_", false},
		{"alnum-mix-ok", "Agent42_", false},
		{"trailing-dash-rejected", "agent-", true},
		{"whitespace-rejected", "agent ", true},
		{"colon-rejected", "agent:", true},
		{"dot-rejected", "agent.", true},
		{"slash-rejected", "agent/", true},
		{"shell-meta-rejected", "$(uname)_", true},
		{"length-63-leaves-1-byte-ok", strings.Repeat("a", maxSessionNameLen-1), false},
		{"length-64-leaves-no-room", strings.Repeat("a", maxSessionNameLen), true},
		{"length-65-rejected", strings.Repeat("a", maxSessionNameLen+1), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSessionPrefix(tc.prefix)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateSessionPrefix(%q) = nil, want error", tc.prefix)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateSessionPrefix(%q) = %v, want nil", tc.prefix, err)
			}
		})
	}
}

// TestResolveSessionRef_Helper tests the prefix-application helper in
// isolation so future refactors of the per-handler call sites can rely
// on a single contract test. Empty input must always pass through (so
// the downstream "session required" validators keep working) and a nil
// receiver must be safe (the test seam used by some unit tests).
func TestResolveSessionRef_Helper(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		prefix string
		in     string
		want   string
	}{
		{"empty-prefix-passthrough", "", "demo", "demo"},
		{"empty-input-passthrough", "ag_", "", ""},
		{"both-empty-passthrough", "", "", ""},
		{"glue-prefix", "ag_", "demo", "ag_demo"},
		{"glue-with-dash-prefix", "ag-alice_", "x", "ag-alice_x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tt := &Tools{SessionPrefix: tc.prefix}
			if got := tt.resolveSessionRef(tc.in); got != tc.want {
				t.Fatalf("resolveSessionRef(%q) under prefix %q = %q, want %q",
					tc.in, tc.prefix, got, tc.want)
			}
		})
	}
	// Nil receiver: must not panic and must not prefix.
	var nilT *Tools
	if got := nilT.resolveSessionRef("demo"); got != "demo" {
		t.Fatalf("nil receiver: resolveSessionRef(\"demo\") = %q, want \"demo\"", got)
	}
}

// TestResolvePaneTarget_Helper covers the pane-target shape rewriter:
// "%N" pane-id strings carry no session reference and pass through;
// bare "session" gets the prefix; "session:window[.pane]" gets the
// prefix on the session half only. A nil receiver and an empty prefix
// are both no-op fast paths.
func TestResolvePaneTarget_Helper(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		prefix string
		in     string
		want   string
	}{
		{"empty-prefix-bare-passthrough", "", "demo", "demo"},
		{"empty-prefix-window-passthrough", "", "demo:0", "demo:0"},
		{"empty-prefix-paneid-passthrough", "", "%5", "%5"},
		{"empty-input-passthrough", "ag_", "", ""},
		{"paneid-passthrough", "ag_", "%5", "%5"},
		{"bare-session-glued", "ag_", "demo", "ag_demo"},
		{"session-window-glued", "ag_", "demo:0", "ag_demo:0"},
		{"session-window-pane-glued", "ag_", "demo:0.1", "ag_demo:0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tt := &Tools{SessionPrefix: tc.prefix}
			if got := tt.resolvePaneTarget(tc.in); got != tc.want {
				t.Fatalf("resolvePaneTarget(%q) under prefix %q = %q, want %q",
					tc.in, tc.prefix, got, tc.want)
			}
		})
	}
	var nilT *Tools
	if got := nilT.resolvePaneTarget("demo:0"); got != "demo:0" {
		t.Fatalf("nil receiver: resolvePaneTarget(\"demo:0\") = %q, want \"demo:0\"", got)
	}
}

// TestResolveWindowMoveTarget_Helper covers the window_move src/dst
// rewriter. Inputs without a colon pass through unchanged so the
// downstream validator emits its existing "must be in <session>:<window>
// form" error; inputs with a colon get the prefix on the session half
// only, including the empty-window form ("session:") which tmux uses
// to mean "next free index".
func TestResolveWindowMoveTarget_Helper(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		prefix string
		in     string
		want   string
	}{
		{"empty-prefix-passthrough", "", "demo:0", "demo:0"},
		{"empty-input-passthrough", "ag_", "", ""},
		{"missing-colon-passthrough", "ag_", "broken", "broken"},
		{"colon-only-glued", "ag_", "demo:", "ag_demo:"},
		{"window-name-glued", "ag_", "demo:work", "ag_demo:work"},
		{"window-index-glued", "ag_", "demo:5", "ag_demo:5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tt := &Tools{SessionPrefix: tc.prefix}
			if got := tt.resolveWindowMoveTarget(tc.in); got != tc.want {
				t.Fatalf("resolveWindowMoveTarget(%q) under prefix %q = %q, want %q",
					tc.in, tc.prefix, got, tc.want)
			}
		})
	}
	var nilT *Tools
	if got := nilT.resolveWindowMoveTarget("demo:0"); got != "demo:0" {
		t.Fatalf("nil receiver: resolveWindowMoveTarget(\"demo:0\") = %q, want \"demo:0\"", got)
	}
}

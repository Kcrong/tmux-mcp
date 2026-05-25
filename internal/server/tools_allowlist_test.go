package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestAllowlist_NilExposesEverything pins the back-compat default: a
// freshly-constructed *Tools with no SetAllowlist call surfaces every
// registered tool via tools/list, and every tool is dispatchable. This
// is what unrelated deployments see when -allowlist is left at its
// empty-string default.
func TestAllowlist_NilExposesEverything(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx := context.Background()

	res, rerr := tools.Handle(ctx, "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	if len(listing) < 5 {
		t.Fatalf("expected several tools listed under no allowlist, got %d", len(listing))
	}
	// session_kill is one of the tools the prompt mentions could be
	// blocked by an allowlist; without one it must dispatch normally
	// (we expect tmux to return an error for the bogus name, not the
	// allowlist guard rejecting the call before tmux runs).
	params := mustJSON(t, map[string]any{
		"name":      "session_kill",
		"arguments": map[string]any{"name": "no_such_session_xyzzy"},
	})
	_, callErr := tools.Handle(ctx, "tools/call", params)
	if callErr == nil {
		t.Fatal("expected tmux to surface a session-not-found error")
	}
	if callErr.Code == codeMethodNotFound {
		t.Fatalf("nil allowlist must not block dispatch, got -32601: %s", callErr.Message)
	}
}

// TestAllowlist_FiltersListAndCall is the load-bearing happy-path test:
// SetAllowlist(["capture"]) → tools/list contains exactly one entry
// (capture), and a tools/call for any other tool returns -32601 with
// the documented "tool %q is not in -allowlist" message.
func TestAllowlist_FiltersListAndCall(t *testing.T) {
	t.Parallel()
	tools := &Tools{}
	if err := tools.SetAllowlist([]string{"capture"}); err != nil {
		t.Fatalf("SetAllowlist: %v", err)
	}

	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	if len(listing) != 1 {
		names := make([]string, 0, len(listing))
		for _, def := range listing {
			names = append(names, def["name"].(string))
		}
		t.Fatalf("expected exactly 1 tool, got %d: %v", len(listing), names)
	}
	if got := listing[0]["name"].(string); got != "capture" {
		t.Fatalf("expected sole entry to be capture, got %q", got)
	}

	// session_kill is registered but filtered out — must trip -32601.
	params := mustJSON(t, map[string]any{
		"name":      "session_kill",
		"arguments": map[string]any{"name": "irrelevant"},
	})
	_, callErr := tools.Handle(context.Background(), "tools/call", params)
	if callErr == nil {
		t.Fatal("expected -32601 for filtered tool, got nil error")
	}
	if callErr.Code != codeMethodNotFound {
		t.Fatalf("expected codeMethodNotFound (%d), got %d", codeMethodNotFound, callErr.Code)
	}
	wantPrefix := `tool "session_kill" is not in -allowlist`
	if !strings.Contains(callErr.Message, wantPrefix) {
		t.Fatalf("expected message containing %q, got %q", wantPrefix, callErr.Message)
	}
}

// TestAllowlist_EnforcedBeforeListEnumerated guards the contract that
// allowlist enforcement does not depend on the client first calling
// tools/list. A direct tools/call for a filtered tool — with no
// preceding initialize / tools/list — must still hit -32601. This
// matches MCP's spec: a malicious or naive client that skips
// enumeration must not bypass the policy.
func TestAllowlist_EnforcedBeforeListEnumerated(t *testing.T) {
	t.Parallel()
	tools := &Tools{}
	if err := tools.SetAllowlist([]string{"capture"}); err != nil {
		t.Fatalf("SetAllowlist: %v", err)
	}
	params := mustJSON(t, map[string]any{
		"name":      "kill_all_sessions",
		"arguments": map[string]any{},
	})
	_, callErr := tools.Handle(context.Background(), "tools/call", params)
	if callErr == nil {
		t.Fatal("expected -32601 even without prior tools/list, got nil error")
	}
	if callErr.Code != codeMethodNotFound {
		t.Fatalf("expected codeMethodNotFound (%d), got %d (msg=%q)",
			codeMethodNotFound, callErr.Code, callErr.Message)
	}
}

// TestAllowlist_AllowedToolDispatchesNormally pins the inverse of the
// reject path: a tool that IS on the allowlist must reach its handler.
// We pick session_list because its handler hits tmux on every call —
// when the allowlist filter is wired correctly we expect either a
// success result or a tmux-related error, but never -32601 from the
// allowlist guard. This guards against an over-eager filter that
// rejects everything, including the tools the operator did permit.
func TestAllowlist_AllowedToolDispatchesNormally(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	if err := tools.SetAllowlist([]string{"session_list"}); err != nil {
		t.Fatalf("SetAllowlist: %v", err)
	}
	params := mustJSON(t, map[string]any{
		"name":      "session_list",
		"arguments": map[string]any{},
	})
	_, callErr := tools.Handle(context.Background(), "tools/call", params)
	// session_list either succeeds (no sessions yet → empty list) or
	// surfaces a tmux internal error. Either way the allowlist guard
	// must not produce a -32601 — a permitted tool must reach its
	// handler.
	if callErr != nil && callErr.Code == codeMethodNotFound {
		t.Fatalf("permitted tool must reach handler, got -32601: %s", callErr.Message)
	}
}

// TestAllowlist_UnknownNamesReportedTogether validates the multi-typo
// reporting contract: every unknown name in the input list shows up in
// the error message in input order, with no early-exit on the first
// typo. Operators typing a long allowlist into a unit file see all
// their typos at once.
func TestAllowlist_UnknownNamesReportedTogether(t *testing.T) {
	t.Parallel()
	tools := &Tools{}
	err := tools.SetAllowlist([]string{"capture", "ghost1", "session_kill", "ghost2"})
	if err == nil {
		t.Fatal("expected error for unknown tools, got nil")
	}
	if !strings.Contains(err.Error(), "unknown tools in -allowlist") {
		t.Fatalf("expected canonical prefix, got %q", err.Error())
	}
	for _, want := range []string{"ghost1", "ghost2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to mention %q, got %q", want, err.Error())
		}
	}
	// State must NOT have been flipped: a follow-up tools/list with the
	// same instance still surfaces every tool because the prior
	// (zero-value, nil) allowlist is preserved on validation failure.
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list after failed SetAllowlist: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	if len(listing) < 5 {
		t.Fatalf("validation failure must not flip state; got only %d tools", len(listing))
	}
}

// TestAllowlist_TrimsAndDeduplicates pins the input-cleaning contract:
// whitespace around individual names is stripped, empty entries (e.g.
// from trailing commas) are skipped silently, and duplicates collapse
// to one entry. This keeps the CSV parser in main.go simple — it just
// splits on "," — without pushing presentation details into every
// caller.
func TestAllowlist_TrimsAndDeduplicates(t *testing.T) {
	t.Parallel()
	tools := &Tools{}
	// A combination of leading/trailing whitespace, an empty entry from
	// a trailing comma, and a duplicate.
	if err := tools.SetAllowlist([]string{" capture ", "", "capture", "session_list"}); err != nil {
		t.Fatalf("SetAllowlist: %v", err)
	}
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	if len(listing) != 2 {
		names := make([]string, 0, len(listing))
		for _, def := range listing {
			names = append(names, def["name"].(string))
		}
		t.Fatalf("expected 2 entries (deduped + trimmed), got %d: %v", len(listing), names)
	}
	got := map[string]bool{}
	for _, def := range listing {
		got[def["name"].(string)] = true
	}
	for _, want := range []string{"capture", "session_list"} {
		if !got[want] {
			t.Errorf("expected %q in tools/list, got %v", want, got)
		}
	}
}

// TestAllowlist_EmptyClearsFilter pins the contract that calling
// SetAllowlist with an empty slice (or a slice that consists entirely
// of blank entries) reverts to the unfiltered default. This is the
// path main.go would take if a future flag rewrite let the operator
// flip the filter at runtime; today's CLI just skips the call when the
// flag is empty, but the API must support a clean reset.
func TestAllowlist_EmptyClearsFilter(t *testing.T) {
	t.Parallel()
	tools := &Tools{}
	if err := tools.SetAllowlist([]string{"capture"}); err != nil {
		t.Fatalf("SetAllowlist initial: %v", err)
	}
	// Reset.
	if err := tools.SetAllowlist(nil); err != nil {
		t.Fatalf("SetAllowlist reset: %v", err)
	}
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	if len(listing) < 5 {
		t.Fatalf("expected unfiltered surface after reset, got %d", len(listing))
	}
}

// TestAllowlist_ValidatesAgainstDynRegistry guards the "future tools
// are picked up automatically" promise: a tool registered via
// RegisterTool (i.e. not part of the static toolDefs literal) must be
// a valid name for SetAllowlist. Without this, the validator would
// silently reject every dynamically-registered tool — exactly the
// failure mode the spec'd "live registry" wording was meant to prevent.
func TestAllowlist_ValidatesAgainstDynRegistry(t *testing.T) {
	t.Parallel()
	tools := &Tools{}
	tools.RegisterTool(
		map[string]any{
			"name":        "custom_dyn_tool",
			"description": "for tests",
			"inputSchema": map[string]any{"type": "object"},
		},
		func(_ context.Context, _ json.RawMessage) (any, *rpcError) {
			return textBlock("ok"), nil
		},
	)
	if err := tools.SetAllowlist([]string{"custom_dyn_tool"}); err != nil {
		t.Fatalf("dynamic tool must be a valid allowlist entry, got %v", err)
	}
}

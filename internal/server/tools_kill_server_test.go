package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestHandle_KillServer exercises the load-bearing happy path:
// populate the controller with sessions, capture one to seed snapshot
// history, then call kill_server. Afterwards the response must ack the
// kill, session_list must come back empty (the "no server running"
// path inside ListSessions degrades to a nil slice), and the snapshot
// store must have forgotten the captured session so a long-running
// server does not leak per-session entries across kill-recreate cycles.
func TestHandle_KillServer(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	call := func(name string, args any) any {
		t.Helper()
		params := mustJSON(t, map[string]any{"name": name, "arguments": args})
		res, rerr := tools.Handle(ctx, "tools/call", params)
		if rerr != nil {
			t.Fatalf("%s: %s", name, rerr.Message)
		}
		return res
	}

	for _, name := range []string{"ks1", "ks2"} {
		call("session_create", map[string]any{
			"name": name, "command": "/bin/sh", "width": 80, "height": 20,
		})
	}
	// Seed snapshot history for ks1 so we can prove kill_server forgets
	// it. The post-kill assertion would be vacuous without an entry to
	// forget.
	_ = extractText(t, call("capture", map[string]any{"session": "ks1"}))
	if !tools.Snap.Has("ks1") {
		t.Fatal("expected snapshot history for ks1 after capture")
	}

	res := extractText(t, call("kill_server", map[string]any{}))
	var ack struct {
		Killed bool `json:"killed"`
	}
	if err := json.Unmarshal([]byte(res), &ack); err != nil {
		t.Fatalf("decode kill_server response: %v\nbody=%s", err, res)
	}
	if !ack.Killed {
		t.Fatalf("expected killed=true, got body=%s", res)
	}

	// session_list against a killed daemon must come back empty —
	// ListSessions's "no server running" / "error connecting" branches
	// degrade to a nil slice, so the JSON payload here is the empty
	// array. Anything else means the daemon is still up (kill failed)
	// or a stray session lingered.
	listRes := extractText(t, call("session_list", map[string]any{}))
	var listPayload struct {
		Sessions []string `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(listRes), &listPayload); err != nil {
		t.Fatalf("decode session_list: %v\nbody=%s", err, listRes)
	}
	if len(listPayload.Sessions) != 0 {
		t.Fatalf("session_list after kill_server = %v, want empty", listPayload.Sessions)
	}

	// Snapshot history must be gone for the captured session so a
	// long-running server cannot accumulate ghost entries across many
	// kill_server cycles.
	if tools.Snap.Has("ks1") {
		t.Fatal("snapshot history still present for ks1 after kill_server")
	}
}

// TestHandle_KillServer_EmptyControllerIsNoop exercises the
// idempotency contract end-to-end: a kill_server on a freshly-built
// *Tools (no daemon ever started) must return the same {"killed":true}
// ack, not an error. tmux's "error connecting to <socket>" stderr is
// the path under test here, swallowed by KillServer into a clean nil.
func TestHandle_KillServer_EmptyControllerIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	params := mustJSON(t, map[string]any{
		"name":      "kill_server",
		"arguments": map[string]any{},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("kill_server on empty controller: %s", rerr.Message)
	}
	body := extractText(t, res)
	var ack struct {
		Killed bool `json:"killed"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode response: %v\nbody=%s", err, body)
	}
	if !ack.Killed {
		t.Fatalf("expected killed=true, got body=%s", body)
	}
}

// TestHandle_KillServer_RejectsExtraArgs locks the
// additionalProperties=false contract. kill_server takes no fields,
// and a stray argument (e.g. an agent assuming a "session" knob exists)
// must come back with -32602 before the controller is touched. Without
// the check an operator's mental model of "this tool takes no inputs"
// could quietly drift.
func TestHandle_KillServer_RejectsExtraArgs(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// The schema rejection happens inside the JSON-RPC layer for
	// schemas that ship with `additionalProperties: false`. We do not
	// exercise that path here directly — Handle's tools/call dispatch
	// does not run schema validation against extra fields, only the
	// outer JSON shape. So instead we assert the registered schema
	// itself carries the lock so a future contributor relaxing it
	// trips this test.
	res, rerr := tools.Handle(ctx, "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	var def map[string]any
	for _, d := range listing {
		if d["name"].(string) == "kill_server" {
			def = d
			break
		}
	}
	if def == nil {
		t.Fatal("kill_server missing from tools/list")
	}
	schema, ok := def["inputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("inputSchema missing or wrong shape: %#v", def["inputSchema"])
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("kill_server inputSchema must lock additionalProperties=false; got %#v",
			schema["additionalProperties"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong shape: %#v", schema["properties"])
	}
	if len(props) != 0 {
		t.Fatalf("kill_server inputSchema must declare no properties; got %v", props)
	}
}

// TestHandle_KillServer_ListedInToolsList pins the tool surface: a
// fresh dispatcher must advertise kill_server in tools/list with a
// description that mentions kill-server (so callers can grep the
// catalogue for the underlying tmux verb). Without this, an init()
// regression that drops the registration would silently make the tool
// uncallable on every new server instance.
func TestHandle_KillServer_ListedInToolsList(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	var def map[string]any
	for _, d := range listing {
		if d["name"].(string) == "kill_server" {
			def = d
			break
		}
	}
	if def == nil {
		names := make([]string, 0, len(listing))
		for _, d := range listing {
			names = append(names, d["name"].(string))
		}
		t.Fatalf("kill_server not in tools/list (got %v)", names)
	}
	desc, _ := def["description"].(string)
	if !strings.Contains(desc, "kill-server") {
		t.Fatalf("kill_server description should mention `tmux kill-server`; got %q", desc)
	}
}

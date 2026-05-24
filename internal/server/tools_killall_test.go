package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestHandle_KillAllSessions creates two sessions, populates snapshot
// history for one of them, then asserts kill_all_sessions wipes the
// session list and forgets snapshot history for every killed name.
func TestHandle_KillAllSessions(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
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

	for _, name := range []string{"ka1", "ka2"} {
		call("session_create", map[string]any{
			"name": name, "command": "/bin/sh", "width": 80, "height": 20,
		})
	}

	// Capture once to populate snapshot history for ka1, then assert
	// the store actually has an entry so the post-kill check is meaningful.
	_ = extractText(t, call("capture", map[string]any{"session": "ka1"}))
	if !tools.Snap.Has("ka1") {
		t.Fatal("expected snapshot history for ka1 after capture")
	}

	res := extractText(t, call("kill_all_sessions", map[string]any{}))
	var payload struct {
		Killed []string `json:"killed"`
		Count  int      `json:"count"`
	}
	if err := json.Unmarshal([]byte(res), &payload); err != nil {
		t.Fatalf("decode kill_all_sessions response: %v\nbody=%s", err, res)
	}
	if payload.Count != 2 {
		t.Fatalf("count = %d, want 2 (killed=%v)", payload.Count, payload.Killed)
	}
	if len(payload.Killed) != 2 {
		t.Fatalf("killed = %v, want 2 entries", payload.Killed)
	}
	got := map[string]bool{}
	for _, n := range payload.Killed {
		got[n] = true
	}
	for _, want := range []string{"ka1", "ka2"} {
		if !got[want] {
			t.Fatalf("killed list missing %q (got %v)", want, payload.Killed)
		}
	}

	// Snapshot history for every killed name must be gone.
	for _, name := range payload.Killed {
		if tools.Snap.Has(name) {
			t.Fatalf("snapshot history still present for killed session %q", name)
		}
	}

	// session_list must report an empty set after kill_all_sessions.
	listRes := extractText(t, call("session_list", map[string]any{}))
	var listPayload struct {
		Sessions []string `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(listRes), &listPayload); err != nil {
		t.Fatalf("decode session_list: %v\nbody=%s", err, listRes)
	}
	if len(listPayload.Sessions) != 0 {
		t.Fatalf("session_list after kill_all = %v, want empty", listPayload.Sessions)
	}
}

// TestHandle_KillAllSessions_EmptyController exercises the no-sessions
// branch end-to-end: the handler must return a valid response (count=0,
// empty killed slice) rather than erroring out.
func TestHandle_KillAllSessions_EmptyController(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	params := mustJSON(t, map[string]any{
		"name":      "kill_all_sessions",
		"arguments": map[string]any{},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("kill_all_sessions on empty controller: %s", rerr.Message)
	}
	body := extractText(t, res)
	var payload struct {
		Killed []string `json:"killed"`
		Count  int      `json:"count"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode response: %v\nbody=%s", err, body)
	}
	if payload.Count != 0 {
		t.Fatalf("count = %d, want 0", payload.Count)
	}
	if len(payload.Killed) != 0 {
		t.Fatalf("killed = %v, want empty", payload.Killed)
	}
}

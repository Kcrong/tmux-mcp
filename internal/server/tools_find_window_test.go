package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// findWindowMatches decodes the boundary's `{"matches": [...]}` text
// block into the structured form the assertions need. Centralising the
// decode keeps the find_window tests focused on assertions about the
// matching behaviour rather than re-stating the JSON shape.
func findWindowMatches(t *testing.T, body string) []map[string]any {
	t.Helper()
	var obj struct {
		Matches []map[string]any `json:"matches"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode find_window: %v\nbody=%s", err, body)
	}
	return obj.Matches
}

// TestHandle_FindWindow_MatchesByName drives the default-scope happy
// path through the dispatcher: the boundary must accept the documented
// arguments, forward them onto the controller, and serialise the
// result into the `{session, window_index, window_name}` shape the
// schema promises.
func TestHandle_FindWindow_MatchesByName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "fwh", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "fwh"}}))
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "fwh", "name": "needle_win", "command": "/bin/sh", "select": false,
	})

	body := extractText(t, callTool(t, tools, ctx, "find_window", map[string]any{
		"match": "needle", "name_only": true, "target": "fwh",
	}))
	got := findWindowMatches(t, body)
	if len(got) != 1 {
		t.Fatalf("matches len = %d, want 1; body=%s", len(got), body)
	}
	if name, _ := got[0]["window_name"].(string); name != "needle_win" {
		t.Errorf("window_name = %q, want needle_win", got[0]["window_name"])
	}
	if sess, _ := got[0]["session"].(string); sess != "fwh" {
		t.Errorf("session = %q, want fwh", got[0]["session"])
	}
	// JSON numbers decode as float64 — coerce explicitly so the test
	// is not sensitive to encoder choices.
	if idx, _ := got[0]["window_index"].(float64); int(idx) < 1 {
		t.Errorf("window_index = %v, want >= 1 (CreateWindow appended past 0)", got[0]["window_index"])
	}
}

// TestHandle_FindWindow_NoMatchSerialisesEmptyArray pins the contract
// that "no matches" round-trips as an empty JSON array, not as null.
// Agents that branch on `matches.length === 0` should never have to
// also handle a null shape; without this pin a slip from `[]` to
// `null` in the encoder would silently break those callers.
func TestHandle_FindWindow_NoMatchSerialisesEmptyArray(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "fwn", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "fwn"}}))
	})

	body := extractText(t, callTool(t, tools, ctx, "find_window", map[string]any{
		"match": "definitely_not_here_xyzzy", "name_only": true, "target": "fwn",
	}))
	if !strings.Contains(body, `"matches":[]`) {
		t.Fatalf("expected empty array shape `\"matches\":[]`, got %s", body)
	}
	got := findWindowMatches(t, body)
	if len(got) != 0 {
		t.Fatalf("matches len = %d, want 0; body=%s", len(got), body)
	}
}

// TestHandle_FindWindow_RejectsMissingMatch locks the up-front
// required-field check. Schema validation already rejects a missing
// `match`, but the runtime check guards against a hand-built
// tools/call that bypasses schema enforcement (every other tool in
// this surface has the same belt-and-braces guard on required strings).
func TestHandle_FindWindow_RejectsMissingMatch(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	params := mustJSON(t, map[string]any{
		"name":      "find_window",
		"arguments": map[string]any{"name_only": true},
	})
	_, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatal("expected -32602 for missing match arg")
	}
	if rerr.Code != codeInvalidParams {
		t.Fatalf("error code = %d, want %d (CodeInvalidParams)", rerr.Code, codeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "match required") {
		t.Fatalf("error message = %q, want it to mention `match required`", rerr.Message)
	}
}

// TestHandle_FindWindow_MissingTargetSurfacesSentinel pins the
// JSON-RPC error mapping for an unknown target session: the
// errs.ErrSessionNotFound the controller wraps must propagate up to
// CodeSessionNotFound (-32000) so callers can branch on the code.
func TestHandle_FindWindow_MissingTargetSurfacesSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor so tmux server is up and the failure is "server up,
	// session missing" rather than "no server running".
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "fwa", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "fwa"}}))
	})

	params := mustJSON(t, map[string]any{
		"name": "find_window",
		"arguments": map[string]any{
			"match": "anything", "target": "ghost_session_nonexistent",
		},
	})
	_, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error for missing target session")
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("error code = %d, want %d (CodeSessionNotFound)", rerr.Code, errs.CodeSessionNotFound)
	}
}

// TestHandle_FindWindow_RegisteredAndAdvertised pins the wire surface:
// find_window must appear in the tools/list response with the
// documented schema fields. Catches a regression where the init() in
// tools_find_window.go fails to append onto toolDefs.
func TestHandle_FindWindow_RegisteredAndAdvertised(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx := context.Background()

	res, rerr := tools.Handle(ctx, "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	var found map[string]any
	for _, def := range listing {
		if def["name"] == "find_window" {
			found = def
			break
		}
	}
	if found == nil {
		t.Fatal("tools/list missing find_window entry")
	}
	schema, ok := found["inputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("find_window inputSchema not a map: %#v", found["inputSchema"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("find_window properties not a map: %#v", schema["properties"])
	}
	for _, want := range []string{
		"match", "regex", "name_only", "title_only", "content_only", "target",
	} {
		if _, ok := props[want]; !ok {
			t.Errorf("schema.properties missing %q", want)
		}
	}
	required, _ := schema["required"].([]string)
	sawMatch := false
	for _, r := range required {
		if r == "match" {
			sawMatch = true
		}
	}
	if !sawMatch {
		t.Errorf("schema.required missing 'match'; got %v", required)
	}
}

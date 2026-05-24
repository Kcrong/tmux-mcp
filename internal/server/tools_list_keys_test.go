package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_ListKeys_DefaultListingHasBindings is the load-bearing
// happy-path through the dispatcher: a fresh tmux server's default key
// map carries dozens of bindings, so the unscoped tool call must
// surface a non-empty `keys` array. A regression where the dispatcher
// forgot to wire the case (or the schema's additionalProperties strict
// mode rejected the empty payload) would trip this first.
func TestHandle_ListKeys_DefaultListingHasBindings(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the controller's tmux server is
	// definitely up. list-keys does not auto-spawn the daemon on every
	// version, so we want a known-running server before the call.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lk_anchor", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "list_keys", map[string]any{}))
	var obj struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_keys: %v\nbody=%s", err, body)
	}
	if len(obj.Keys) == 0 {
		t.Fatalf("expected at least one binding on a default tmux install, got 0 (%s)", body)
	}
	// Spot-check the wire shape: every entry must carry the documented
	// {table, key, command} keys with non-empty values, so a future
	// refactor that drops a column shows up here instead of as a
	// silent regression on the agent side.
	for i, kb := range obj.Keys {
		for _, k := range []string{"table", "key", "command"} {
			v, ok := kb[k].(string)
			if !ok || v == "" {
				t.Fatalf("keys[%d] field %q empty/missing: %#v", i, k, kb)
			}
		}
	}
}

// TestHandle_ListKeys_KeyTableScoped pins the `key_table` arg: scoping
// to "prefix" must return strictly fewer entries than the unscoped
// listing and every entry's table column must equal "prefix". This
// guards both the boundary's `-T` forwarding and the schema's plumbing
// of the `key_table` JSON field.
func TestHandle_ListKeys_KeyTableScoped(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lk_scope", "command": "/bin/sh",
	})

	all := decodeKeys(t, callTool(t, tools, ctx, "list_keys", map[string]any{}))
	prefix := decodeKeys(t, callTool(t, tools, ctx, "list_keys", map[string]any{"key_table": "prefix"}))

	if len(prefix) == 0 {
		t.Fatal("expected non-empty prefix-table listing")
	}
	if len(prefix) >= len(all) {
		t.Fatalf("prefix (%d) should be smaller than all (%d)", len(prefix), len(all))
	}
	for i, kb := range prefix {
		if kb["table"] != "prefix" {
			t.Fatalf("prefix[%d].table = %q, want \"prefix\"", i, kb["table"])
		}
	}
}

// TestHandle_ListKeys_NotesOnlyShrinks pins the `notes_only` arg: with
// it set to true the response must be strictly smaller than the
// default listing — `-N` mode hides every binding without a note. The
// command column carries the note text rather than a tmux command,
// but the wire shape stays uniform so callers don't need a separate
// branch.
func TestHandle_ListKeys_NotesOnlyShrinks(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lk_notes", "command": "/bin/sh",
	})

	all := decodeKeys(t, callTool(t, tools, ctx, "list_keys", map[string]any{}))
	notes := decodeKeys(t, callTool(t, tools, ctx, "list_keys", map[string]any{"notes_only": true}))

	if len(notes) == 0 {
		t.Fatal("expected at least one annotated binding in the default tmux key map")
	}
	if len(notes) >= len(all) {
		t.Fatalf("notes-only (%d) should be smaller than all (%d)", len(notes), len(all))
	}
}

// TestHandle_ListKeys_PrefixForwardedToOutput confirms the `prefix`
// arg is forwarded as `-P PREFIX`: in notes-only mode every rendered
// key chord should start with the prefix the caller supplied, so a
// regression where the boundary swallowed the flag surfaces immediately.
func TestHandle_ListKeys_PrefixForwardedToOutput(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lk_prefix", "command": "/bin/sh",
	})

	const wantPrefix = "PFX "
	keys := decodeKeys(t, callTool(t, tools, ctx, "list_keys", map[string]any{
		"notes_only": true,
		"prefix":     wantPrefix,
	}))
	if len(keys) == 0 {
		t.Fatal("expected at least one entry in notes-only listing")
	}
	for i, kb := range keys {
		k, _ := kb["key"].(string)
		if len(k) < len(wantPrefix) || k[:len(wantPrefix)] != wantPrefix {
			t.Fatalf("keys[%d].key = %q, want prefix %q", i, k, wantPrefix)
		}
	}
}

// TestHandle_ListKeys_AcceptsEmptyArguments guards the "raw is empty"
// branch — the dispatcher hands list_keys a nil-ish payload when the
// caller sends `arguments: {}`. The handler must accept it as "every
// binding in every table, default rendering" rather than rejecting it
// as malformed (mirrors list_clients / list_buffers behaviour).
func TestHandle_ListKeys_AcceptsEmptyArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lk_empty", "command": "/bin/sh",
	})

	// Construct params manually so we can omit the "arguments" key
	// entirely — that's the path that exercises the len(raw) == 0
	// branch in the handler.
	params := mustJSON(t, map[string]any{"name": "list_keys"})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("list_keys: %s", rerr.Message)
	}
	body := extractText(t, res)
	var obj struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_keys: %v\nbody=%s", err, body)
	}
}

// TestHandle_ListKeys_RejectsBadKeyTable guards the regex/length
// policy on the optional `key_table` argument — even though it's
// optional, a present-but-malformed value must still be refused with
// CodeInvalidParams up front so tmux is never asked to resolve it
// (defence against shell metachars / accidentally-quoted input).
func TestHandle_ListKeys_RejectsBadKeyTable(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "list_keys",
		"arguments": map[string]any{"key_table": "bad table with spaces"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad key_table")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ListKeys_RejectsTooLongPrefix pins the upper bound on
// the optional `prefix` argument. Without the cap an agent could
// smuggle a megabyte payload onto tmux's argv before any boundary
// runs.
func TestHandle_ListKeys_RejectsTooLongPrefix(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	huge := make([]byte, maxKeyPrefixLen+1)
	for i := range huge {
		huge[i] = 'x'
	}
	params := mustJSON(t, map[string]any{
		"name":      "list_keys",
		"arguments": map[string]any{"prefix": string(huge)},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized prefix")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ListKeys_AdditionalPropertiesLocked enforces the
// additionalProperties:false contract on the schema — a typo like
// "table" instead of "key_table" must surface in the schema rather
// than being silently swallowed at decode time. We assert on the
// schema entry directly because the handler's typed struct already
// drops unknown JSON fields.
func TestHandle_ListKeys_AdditionalPropertiesLocked(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "list_keys" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("list_keys schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		return
	}
	t.Fatal("tools/list missing list_keys")
}

// TestHandle_ToolsList_IncludesListKeys makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide
// the tool from the surface even though the dispatcher case still
// works for a hardcoded call.
func TestHandle_ToolsList_IncludesListKeys(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "list_keys" {
			return
		}
	}
	t.Fatal("tools/list missing list_keys")
}

// decodeKeys is a small helper that pulls the list of {table, key,
// command} maps out of a list_keys response. It keeps the per-test
// boilerplate down without hiding the JSON shape — the assertions
// still see the raw wire form a real client would consume.
func decodeKeys(t *testing.T, result any) []map[string]any {
	t.Helper()
	body := extractText(t, result)
	var obj struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_keys: %v\nbody=%s", err, body)
	}
	return obj.Keys
}

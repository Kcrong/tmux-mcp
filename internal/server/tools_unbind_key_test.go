package server

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// bindKeyForTest installs a fresh binding on the controller's tmux
// daemon by shelling out to `tmux -S <socket> bind-key -T TABLE KEY ...`
// directly. The unbind_key tool ships before its sister `bind_key`
// tool, so we cannot drive the bind through the public dispatcher
// surface; reaching for the tmux CLI keeps the test self-contained
// without coupling to either the not-yet-merged bind_key tool or a
// test-only Controller export. The bound action is `display-message`
// so a chord that ever fires only logs a noop.
func bindKeyForTest(t *testing.T, tools *Tools, ctx context.Context, table, key string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "tmux",
		"-S", tools.Ctl.Socket(),
		"bind-key", "-T", table, key,
		"display-message", "noop",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bind-key %s/%s: %v: %s", table, key, err, string(out))
	}
}

// keyExistsInListKeys drives the list_keys tool through the dispatcher
// and reports whether a chord/table pair is currently bound. We go
// through the public surface (rather than reaching into the controller
// directly) because that is the same surface a real client would
// observe — a regression where the unbind landed on tmux but the
// listing tool stopped reflecting it would still trip this probe.
//
// "table doesn't exist" comes back as a CodeInternal from list_keys
// (tmux deletes a custom key table once its last binding is removed),
// which at the unbind contract level is observationally identical to
// "the chord is no longer bound". The helper translates that shape
// into "absent" so the post-condition check stays focused on the
// thing under test.
func keyExistsInListKeys(t *testing.T, tools *Tools, ctx context.Context, table, key string) bool {
	t.Helper()
	params := mustJSON(t, map[string]any{
		"name":      "list_keys",
		"arguments": map[string]any{"key_table": table},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		if rerr.Code == errs.CodeInternal && containsTableMissing(rerr.Message) {
			return false
		}
		t.Fatalf("list_keys: %s", rerr.Message)
	}
	body := extractText(t, res)
	var obj struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_keys: %v\nbody=%s", err, body)
	}
	for _, kb := range obj.Keys {
		if k, _ := kb["key"].(string); k == key {
			return true
		}
	}
	return false
}

// containsTableMissing matches tmux's "table TABLE doesn't exist"
// stderr shape case-insensitively. Pulled into a helper so both the
// happy-path and all-variant tests share the recognition logic.
func containsTableMissing(s string) bool {
	return strings.Contains(strings.ToLower(s), "doesn't exist")
}

// TestHandle_UnbindKey_HappyPath drives the dispatcher end-to-end:
// install a fresh binding, ask unbind_key to remove it via tools/call,
// observe it disappears from list_keys. This pins both the dispatcher
// case wiring and the controller's `-T TABLE KEY` argument shape — a
// regression where the boundary dropped either flag would surface as
// the post-unbind probe still finding the chord.
func TestHandle_UnbindKey_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	// Anchor the daemon so list-keys / bind-key target a live server.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "uk_hp", "command": "/bin/sh",
	})

	const table = "uk-hp-table"
	const key = "F8"
	bindKeyForTest(t, tools, ctx, table, key)
	if !keyExistsInListKeys(t, tools, ctx, table, key) {
		t.Fatalf("pre-condition: %s/%s not present in list_keys after bind", table, key)
	}

	res := callTool(t, tools, ctx, "unbind_key", map[string]any{
		"key": key, "key_table": table,
	})
	body := extractText(t, res)
	var ack struct {
		Unbound bool `json:"unbound"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode unbind_key response: %v\nbody=%s", err, body)
	}
	if !ack.Unbound {
		t.Fatalf("expected unbound=true, got body=%s", body)
	}
	if keyExistsInListKeys(t, tools, ctx, table, key) {
		t.Fatalf("post-condition: %s/%s still in list_keys after unbind_key", table, key)
	}
}

// TestHandle_UnbindKey_AllVariant exercises the `all=true` shape: bind
// two keys in the same table, ask unbind_key with all=true, both
// vanish. Catches a regression where the boundary forgot to forward
// `-a` onto argv (the unbind would silently miss every key).
func TestHandle_UnbindKey_AllVariant(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "uk_all", "command": "/bin/sh",
	})

	const table = "uk-all-table"
	bindKeyForTest(t, tools, ctx, table, "F7")
	bindKeyForTest(t, tools, ctx, table, "F6")
	for _, k := range []string{"F7", "F6"} {
		if !keyExistsInListKeys(t, tools, ctx, table, k) {
			t.Fatalf("pre-condition: %s/%s not present after bind", table, k)
		}
	}

	res := callTool(t, tools, ctx, "unbind_key", map[string]any{
		"key_table": table, "all": true,
	})
	body := extractText(t, res)
	var ack struct {
		Unbound bool `json:"unbound"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode unbind_key all=true response: %v\nbody=%s", err, body)
	}
	if !ack.Unbound {
		t.Fatalf("expected unbound=true, got body=%s", body)
	}
	for _, k := range []string{"F7", "F6"} {
		if keyExistsInListKeys(t, tools, ctx, table, k) {
			t.Fatalf("post-condition: %s/%s still present after unbind_key all=true", table, k)
		}
	}
}

// TestHandle_UnbindKey_Idempotent pins the double-unbind contract
// end-to-end: a second unbind_key on a chord that is no longer bound
// must still return {"unbound":true}, not surface a tmux error.
// Mirrors the idempotent-by-design behaviour of the underlying tmux
// command — agents whose recovery loops re-issue the same setup
// frame must not see a spurious failure on the second iteration.
func TestHandle_UnbindKey_Idempotent(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "uk_idem", "command": "/bin/sh",
	})

	const table = "uk-idem-table"
	const key = "F5"
	bindKeyForTest(t, tools, ctx, table, key)

	// First call removes the binding.
	_ = callTool(t, tools, ctx, "unbind_key", map[string]any{
		"key": key, "key_table": table,
	})
	// Second call against an already-unbound chord must still ack.
	res := callTool(t, tools, ctx, "unbind_key", map[string]any{
		"key": key, "key_table": table,
	})
	body := extractText(t, res)
	var ack struct {
		Unbound bool `json:"unbound"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode idempotent unbind_key response: %v\nbody=%s", err, body)
	}
	if !ack.Unbound {
		t.Fatalf("expected unbound=true on idempotent call, got body=%s", body)
	}
}

// TestHandle_UnbindKey_RejectsBothEmpty pins the {key xor all=true}
// validation: a call with neither `key` nor `all=true` would silently
// no-op on tmux and is treated as a programmer error here. Without
// the guard a buggy caller would see a successful no-op and never
// realise their unbind never landed.
func TestHandle_UnbindKey_RejectsBothEmpty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "unbind_key",
		"arguments": map[string]any{},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty key + all=false")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_UnbindKey_RejectsBothSet pins the inverse contradiction:
// `all=true` (wipe) plus a `key` (remove just this one) does not have
// a single well-defined meaning, and tmux silently swallows the KEY.
// The boundary refuses the shape so callers get a clean -32602 instead
// of a confusing successful-but-wrong outcome.
func TestHandle_UnbindKey_RejectsBothSet(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "unbind_key",
		"arguments": map[string]any{
			"key": "C-a", "all": true,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for key + all=true")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_UnbindKey_RejectsBadKeyTable guards the regex/length
// policy on the optional `key_table` argument. A malformed value must
// be refused with CodeInvalidParams up front so tmux is never asked to
// resolve it (defence against shell metachars / accidentally-quoted
// input).
func TestHandle_UnbindKey_RejectsBadKeyTable(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "unbind_key",
		"arguments": map[string]any{
			"key": "C-a", "key_table": "bad table with spaces",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad key_table")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_UnbindKey_RejectsControlByteInKey pins the NUL/control-
// byte policy: a key containing a NUL or ASCII control byte (other
// than DEL) must be refused. tmux keysyms are short ASCII tokens; a
// caller smuggling control bytes through is almost certainly malicious
// or buggy, and refusing the input here keeps tmux's argv clean.
func TestHandle_UnbindKey_RejectsControlByteInKey(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "unbind_key",
		"arguments": map[string]any{
			"key": "C-a\x01evil",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for key with control byte")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_UnbindKey_RejectsTooLongKey pins the upper bound on the
// `key` argument so an agent cannot smuggle a megabyte payload onto
// tmux's argv before the boundary validates anything else.
func TestHandle_UnbindKey_RejectsTooLongKey(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	huge := make([]byte, maxUnbindKeyLen+1)
	for i := range huge {
		huge[i] = 'a'
	}
	params := mustJSON(t, map[string]any{
		"name": "unbind_key",
		"arguments": map[string]any{
			"key": string(huge),
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized key")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_UnbindKey_AdditionalPropertiesLocked enforces the
// additionalProperties:false contract on the schema — a typo like
// "table" instead of "key_table" must surface through the schema
// rather than being silently swallowed at decode time.
func TestHandle_UnbindKey_AdditionalPropertiesLocked(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] != "unbind_key" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("unbind_key schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		props, _ := schema["properties"].(map[string]any)
		for _, want := range []string{"key", "key_table", "all"} {
			if _, ok := props[want]; !ok {
				t.Fatalf("unbind_key schema missing property %q", want)
			}
		}
		return
	}
	t.Fatal("tools/list missing unbind_key")
}

// TestHandle_ToolsList_IncludesUnbindKey makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide
// the tool from the surface even though the dispatcher case still
// works for a hardcoded call.
func TestHandle_ToolsList_IncludesUnbindKey(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] == "unbind_key" {
			return
		}
	}
	t.Fatal("tools/list missing unbind_key")
}

// TestHandle_UnbindKey_RejectsUnknownProperty exercises the
// unknown-property arm of the schema's additionalProperties:false
// contract end-to-end. Even though Go's json.Unmarshal does not
// enforce additionalProperties at decode time, the schema is the
// surface clients consume; pinning the metadata here means a future
// contributor relaxing the lock trips this test alongside the
// AdditionalPropertiesLocked one.
func TestHandle_UnbindKey_RejectsUnknownProperty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] != "unbind_key" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, leaked := props["table"]; leaked {
			t.Fatal("unbind_key schema must not expose a `table` property (use `key_table`)")
		}
		if _, leaked := props["unbind_all"]; leaked {
			t.Fatal("unbind_key schema must not expose `unbind_all` (use `all`)")
		}
		return
	}
	t.Fatal("tools/list missing unbind_key")
}

// TestUnbindKey_NotInReadOnlyAllowlist pins the policy: unbind_key
// removes a key binding and is therefore mutating, so a -read-only
// operator must not be able to invoke it. Adding the tool to the
// allowlist would silently let a read-only agent erase bindings they
// only meant to inspect.
func TestUnbindKey_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("unbind_key") {
		t.Fatal("unbind_key must NOT be in the read-only allowlist (it removes bindings)")
	}
}

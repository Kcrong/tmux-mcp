package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_ListCommands_DefaultListingIsLarge is the load-bearing
// happy-path through the dispatcher: every supported tmux release
// advertises dozens of commands, so the unscoped tool call must
// surface a large `commands` array. A regression where the dispatcher
// forgot to wire the case (or the schema's additionalProperties strict
// mode rejected the empty payload) would trip this first.
func TestHandle_ListCommands_DefaultListingIsLarge(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	body := extractText(t, callTool(t, tools, ctx, "list_commands", map[string]any{}))
	cmds := decodeCommands(t, body)
	if len(cmds) < 30 {
		t.Fatalf("expected at least 30 commands on a default tmux install, got %d (%s)", len(cmds), body)
	}
	// Spot-check the wire shape: every entry must carry the documented
	// {name, alias, args} keys with name non-empty, so a future
	// refactor that drops a column shows up here instead of as a
	// silent regression on the agent side. alias and args are allowed
	// to be empty strings (no alias for many commands; no args for
	// kill-server / start-server / lock-server).
	for i, ci := range cmds {
		name, ok := ci["name"].(string)
		if !ok || name == "" {
			t.Fatalf("commands[%d] name empty/missing: %#v", i, ci)
		}
		if _, ok := ci["alias"].(string); !ok {
			t.Fatalf("commands[%d] alias must be a string (possibly empty): %#v", i, ci)
		}
		if _, ok := ci["args"].(string); !ok {
			t.Fatalf("commands[%d] args must be a string (possibly empty): %#v", i, ci)
		}
	}
}

// TestHandle_ListCommands_FilterScopesToOne pins the optional
// `command` arg: scoping to a known verb returns exactly one entry
// whose `name` matches. Picking "list-keys" keeps the test grounded
// against a command we already drive elsewhere on the surface.
func TestHandle_ListCommands_FilterScopesToOne(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	cmds := decodeCommands(t, extractText(t, callTool(t, tools, ctx, "list_commands", map[string]any{
		"command": "list-keys",
	})))
	if len(cmds) != 1 {
		t.Fatalf("expected exactly 1 entry for command=\"list-keys\", got %d: %#v", len(cmds), cmds)
	}
	if got, _ := cmds[0]["name"].(string); got != "list-keys" {
		t.Fatalf("commands[0].name = %q, want list-keys", got)
	}
	if got, _ := cmds[0]["alias"].(string); got != "lsk" {
		t.Fatalf("commands[0].alias = %q, want lsk", got)
	}
}

// TestHandle_ListCommands_FilterUnknownReturnsEmptyArray pins the
// "filter does not match a known command returns empty array" path.
// tmux 3.0–3.3 exits 1 with empty stdout in this case; 3.4+ exits 0
// with empty stdout. The boundary collapses both into a clean empty
// list rather than an error so callers can iterate the response
// without a separate "is this an error" branch.
func TestHandle_ListCommands_FilterUnknownReturnsEmptyArray(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	body := extractText(t, callTool(t, tools, ctx, "list_commands", map[string]any{
		"command": "definitely-not-a-tmux-command-xyzzy",
	}))
	cmds := decodeCommands(t, body)
	if cmds == nil {
		t.Fatalf("expected non-nil commands slice for filter-no-match (body=%s)", body)
	}
	if len(cmds) != 0 {
		t.Fatalf("expected empty commands slice for filter-no-match, got %d: %s", len(cmds), body)
	}
}

// TestHandle_ListCommands_AcceptsEmptyArguments guards the "raw is
// empty" branch — the dispatcher hands list_commands a nil-ish payload
// when the caller sends `arguments: {}` (or omits the field entirely).
// The handler must accept it as "every command, no filter" rather
// than rejecting it as malformed (mirrors list_clients / list_buffers
// / list_keys behaviour).
func TestHandle_ListCommands_AcceptsEmptyArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Construct params manually so we can omit the "arguments" key
	// entirely — that's the path that exercises the len(raw) == 0
	// branch in the handler.
	params := mustJSON(t, map[string]any{"name": "list_commands"})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("list_commands: %s", rerr.Message)
	}
	cmds := decodeCommands(t, extractText(t, res))
	if len(cmds) < 30 {
		t.Fatalf("expected at least 30 commands for empty-arguments call, got %d", len(cmds))
	}
}

// TestHandle_ListCommands_RejectsBadCommandFilter guards the
// regex/length policy on the optional `command` argument — even
// though it's optional, a present-but-malformed value must still be
// refused with CodeInvalidParams up front so tmux is never asked to
// resolve it (defence against shell metachars / accidentally-quoted
// input).
func TestHandle_ListCommands_RejectsBadCommandFilter(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "list_commands",
		"arguments": map[string]any{"command": "bad command with spaces"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad command filter")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ListCommands_RejectsTooLongFilter pins the upper bound
// on the optional `command` argument. Without the cap an agent could
// smuggle a megabyte payload onto tmux's argv before any boundary
// runs.
func TestHandle_ListCommands_RejectsTooLongFilter(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	huge := make([]byte, maxCommandFilterLen+1)
	for i := range huge {
		huge[i] = 'x'
	}
	params := mustJSON(t, map[string]any{
		"name":      "list_commands",
		"arguments": map[string]any{"command": string(huge)},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized command filter")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ListCommands_AdditionalPropertiesLocked enforces the
// additionalProperties:false contract on the schema — a typo like
// "name" instead of "command" must surface in the schema rather than
// being silently swallowed at decode time. We assert on the schema
// entry directly because the handler's typed struct already drops
// unknown JSON fields.
func TestHandle_ListCommands_AdditionalPropertiesLocked(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "list_commands" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("list_commands schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		return
	}
	t.Fatal("tools/list missing list_commands")
}

// TestHandle_ToolsList_IncludesListCommands makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide the
// tool from the surface even though the dispatcher case still works
// for a hardcoded call.
func TestHandle_ToolsList_IncludesListCommands(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "list_commands" {
			return
		}
	}
	t.Fatal("tools/list missing list_commands")
}

// decodeCommands is a small helper that pulls the list of {name,
// alias, args} maps out of a list_commands response. Keeps the
// per-test boilerplate down without hiding the JSON shape — the
// assertions still see the raw wire form a real client would
// consume.
func decodeCommands(t *testing.T, body string) []map[string]any {
	t.Helper()
	var obj struct {
		Commands []map[string]any `json:"commands"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_commands: %v\nbody=%s", err, body)
	}
	return obj.Commands
}

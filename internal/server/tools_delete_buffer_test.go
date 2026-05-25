package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// decodeDeleteBuffer pulls the {"deleted": ..., "name": ...} envelope
// out of the tools/call result so the assertions in each test stay
// focused on the field that matters.
func decodeDeleteBuffer(t *testing.T, result any) (deleted bool, name string) {
	t.Helper()
	body := extractText(t, result)
	var obj struct {
		Deleted bool   `json:"deleted"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode delete_buffer body %q: %v", body, err)
	}
	return obj.Deleted, obj.Name
}

// TestHandle_DeleteBuffer_HappyPath drives the load-bearing path: seed
// two named buffers via `tmux set-buffer -b NAME`, drop one through
// the dispatcher, and confirm tmux's own list-buffers no longer reports
// it (the survivor stays). We probe tmux directly (rather than through
// list_buffers) so a regression in the read-side tools cannot mask a
// regression in delete_buffer.
func TestHandle_DeleteBuffer_HappyPath(t *testing.T) {
	t.Parallel()
	tools, call, _ := bufferTestSetup(t)

	seedBuffer(t, tools, "doomed", "goodbye")
	seedBuffer(t, tools, "survivor", "still_here")

	deleted, name := decodeDeleteBuffer(t, call("delete_buffer", map[string]any{
		"name": "doomed",
	}))
	if !deleted {
		t.Fatalf("delete_buffer deleted=false, want true")
	}
	if name != "doomed" {
		t.Errorf("delete_buffer echoed name = %q, want %q", name, "doomed")
	}

	// Probe tmux directly: list-buffers must no longer list `doomed`,
	// and `survivor` must still be present.
	listing := tmuxRun(t, tools.Ctl.Socket(), "list-buffers", "-F", "#{buffer_name}")
	if strings.Contains(listing, "doomed") {
		t.Errorf("list-buffers still contains doomed; got\n%s", listing)
	}
	if !strings.Contains(listing, "survivor") {
		t.Errorf("list-buffers missing survivor after delete; got\n%s", listing)
	}
}

// TestHandle_DeleteBuffer_MissingMapsCode pins the wire contract for
// "delete a buffer that doesn't exist": the JSON-RPC error code must
// surface as CodeSessionNotFound (-32000) so MCP clients can branch
// on the same stable code show_buffer already uses for the same
// conceptual outcome.
func TestHandle_DeleteBuffer_MissingMapsCode(t *testing.T) {
	t.Parallel()
	tools, _, ctx := bufferTestSetup(t)

	params := mustJSON(t, map[string]any{
		"name":      "delete_buffer",
		"arguments": map[string]any{"name": "ghost_buffer_nonexistent"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for missing buffer, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_DeleteBuffer_RejectsMissingName pins the up-front guard:
// a tools/call with no `name` field (or with `name: ""`) must fail
// with CodeInvalidParams (-32602) before any tmux command runs. The
// MCP boundary deliberately requires `name` rather than mirroring
// tmux's bare `delete-buffer` (no -b) → "drop most-recent" semantics,
// which would invite agents to accidentally destroy buffers another
// caller just minted.
func TestHandle_DeleteBuffer_RejectsMissingName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "delete_buffer",
		"arguments": map[string]any{},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_DeleteBuffer_RejectsEmptyName pins the same guard as
// RejectsMissingName but for an explicit `name: ""`. Both paths must
// fail with CodeInvalidParams so a caller cannot bypass the
// "force the agent to be deterministic" check by sending an empty
// string instead of omitting the field.
func TestHandle_DeleteBuffer_RejectsEmptyName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "delete_buffer",
		"arguments": map[string]any{"name": ""},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_DeleteBuffer_RejectsBadName locks the regex check on
// `name` so a stray quote or whitespace cannot slip through to the
// tmux argv. The check runs before any tmux command, so the error
// must carry CodeInvalidParams (-32602).
func TestHandle_DeleteBuffer_RejectsBadName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "delete_buffer",
		"arguments": map[string]any{"name": "bad name with spaces"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ToolsList_IncludesDeleteBuffer makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint.
func TestHandle_ToolsList_IncludesDeleteBuffer(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "delete_buffer" {
			return
		}
	}
	t.Fatalf("tools/list missing delete_buffer")
}

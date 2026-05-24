package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_DisplayMessage_HappyPath drives the integration: spin up
// a real tmux session, ask `display_message` for `#{session_name}`,
// and assert the response envelope decodes cleanly with the matching
// resolved value. This pins both the wire shape and the actual tmux
// round-trip end-to-end.
func TestHandle_DisplayMessage_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	const name = "dm_rt"
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": name, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": name},
			}))
	})

	body := extractText(t, callTool(t, tools, ctx, "display_message", map[string]any{
		"format":  "#{session_name}",
		"session": name,
	}))

	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode display_message body %q: %v", body, err)
	}
	value, _ := got["value"].(string)
	if value != name {
		t.Errorf("value = %q, want %q", value, name)
	}
}

// TestHandle_DisplayMessage_NoTarget exercises the "all empty" branch:
// when no session/window/pane is supplied the controller omits `-t`
// and tmux resolves the format against its current/global context.
// We pin the surface against the constant-output `#{?TMUX,1,0}`-style
// expression by using a literal — keeping the assertion independent
// of which tmux session happens to be "current" at evaluation time.
func TestHandle_DisplayMessage_NoTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so the display-message
	// invocation has a "current" context to resolve against.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dm_anchor", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "dm_anchor"},
			}))
	})

	body := extractText(t, callTool(t, tools, ctx, "display_message", map[string]any{
		"format": "literal-string-no-vars",
	}))
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode display_message body %q: %v", body, err)
	}
	if value, _ := got["value"].(string); value != "literal-string-no-vars" {
		t.Errorf("value = %q, want %q", value, "literal-string-no-vars")
	}
}

// TestHandle_DisplayMessage_MissingFormat pins the up-front guard:
// without `format` the call must fail with CodeInvalidParams before
// any tmux invocation.
func TestHandle_DisplayMessage_MissingFormat(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name":      "display_message",
		"arguments": map[string]any{},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing format")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestHandle_DisplayMessage_RejectsNewlineInFormat pins the explicit
// rule the task spec calls out: literal newlines are not allowed in
// the format. tmux would otherwise emit multi-line output that breaks
// the schema's "single string" contract.
func TestHandle_DisplayMessage_RejectsNewlineInFormat(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name": "display_message",
		"arguments": map[string]any{
			"format": "first\nsecond",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for newline in format")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
	if !strings.Contains(rerr.Message, "must not contain newlines") {
		t.Errorf("error message = %q, want substring %q",
			rerr.Message, "must not contain newlines")
	}
}

// TestHandle_DisplayMessage_RejectsBadSession guards the regex policy
// on the `session` argument. A string with spaces would otherwise be
// passed straight to tmux.
func TestHandle_DisplayMessage_RejectsBadSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name": "display_message",
		"arguments": map[string]any{
			"format":  "#{session_name}",
			"session": "bad name with spaces",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad session")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestHandle_DisplayMessage_UnknownSessionMapsCode pins the wire
// contract for "evaluate against a session that does not exist": the
// JSON-RPC error code must be CodeSessionNotFound (-32000) so MCP
// clients can branch on a stable code rather than the
// version-specific tmux stderr text.
func TestHandle_DisplayMessage_UnknownSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so the controller
	// hits the "server up, named session missing" branch.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dm_anchor2", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "dm_anchor2"},
			}))
	})

	params := mustJSON(t, map[string]any{
		"name": "display_message",
		"arguments": map[string]any{
			"format":  "#{session_name}",
			"session": "definitely_missing_xyzzy",
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_DisplayMessage_ListedInTools confirms the init()-time
// registration actually wired display_message into tools/list — and
// that the schema locks additionalProperties:false so a typo in
// arguments fails fast.
func TestHandle_DisplayMessage_ListedInTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] != "display_message" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		if schema == nil {
			t.Fatalf("display_message missing inputSchema")
		}
		if got, ok := schema["additionalProperties"].(bool); !ok || got {
			t.Errorf("additionalProperties = %v, want false", schema["additionalProperties"])
		}
		required, _ := schema["required"].([]string)
		// In Go's literal map[string]any with []string the assertion
		// above only succeeds when the literal carried []string; if it
		// carried []any (e.g. round-tripped via JSON) try that shape.
		if len(required) == 0 {
			if anyReq, ok := schema["required"].([]any); ok {
				for _, n := range anyReq {
					if s, _ := n.(string); s != "" {
						required = append(required, s)
					}
				}
			}
		}
		sawFormat := false
		for _, r := range required {
			if r == "format" {
				sawFormat = true
			}
		}
		if !sawFormat {
			t.Errorf("display_message required = %v, want to include \"format\"", required)
		}
		return
	}
	t.Fatal("tools/list missing display_message")
}

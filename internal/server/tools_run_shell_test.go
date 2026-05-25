package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_RunShell_HappyPathReturnsStdout drives the happy path
// end-to-end through the dispatcher: session_create (so the tmux
// server is up) → run_shell → assert the JSON response carries the
// captured stdout. Verifies the dispatcher is wired up, the schema
// accepts the documented arguments, and the response envelope carries
// the `{"stdout": "hello"}` ack.
func TestHandle_RunShell_HappyPathReturnsStdout(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so the tmux server is alive — run-shell
	// otherwise emits "no server running" before reaching /bin/sh.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "rsh", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "rsh"}}))
	})

	body := extractText(t, callTool(t, tools, ctx, "run_shell", map[string]any{
		"command": "printf hello",
	}))
	var resp struct {
		Stdout string `json:"stdout"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode run_shell: %v\nbody=%s", err, body)
	}
	if resp.Stdout != "hello" {
		t.Fatalf("response.stdout = %q, want %q; body=%s", resp.Stdout, "hello", body)
	}
}

// TestHandle_RunShell_BackgroundReturnsEmptyQuickly pins the `-b`
// contract end-to-end: when background=true tmux runs the command
// detached and the response carries an empty stdout. We deliberately
// pass `sleep 30` so a regression where -b is dropped would visibly
// hang this test past the (short) ctx deadline.
func TestHandle_RunShell_BackgroundReturnsEmptyQuickly(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "rshbg", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "rshbg"}}))
	})

	start := time.Now()
	body := extractText(t, callTool(t, tools, ctx, "run_shell", map[string]any{
		"command":    "sleep 30",
		"background": true,
	}))
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("background run_shell blocked %s; -b should return promptly", d)
	}
	var resp struct {
		Stdout string `json:"stdout"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode run_shell: %v\nbody=%s", err, body)
	}
	if resp.Stdout != "" {
		t.Fatalf("background response.stdout = %q, want empty; body=%s", resp.Stdout, body)
	}
}

// TestHandle_RunShell_MissingTargetMapsCode pins the wire contract:
// run_shell against a target on an unknown session must surface
// CodeSessionNotFound (-32000), mirroring pipe_pane / pane_kill /
// clear_history. The has-session probe inside RunShell is what makes
// this stable — without it tmux silently runs the command anyway.
func TestHandle_RunShell_MissingTargetMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so we exercise "server up, target session
	// missing" — the stderr shape changes against "no server".
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "rshanchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "run_shell",
		"arguments": map[string]any{
			"command": "printf hello",
			"target":  "definitely_does_not_exist_xyzzy",
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

// TestHandle_RunShell_RejectsControlChars locks the boundary policy
// on `command`: a NUL byte or other control character (newline, ESC,
// …) must be rejected before any tmux call runs. The command is
// passed verbatim to /bin/sh by tmux, so we are responsible for
// ensuring the bytes that get there can't smuggle past the JSON-RPC
// frame or tmux's own argv parser.
func TestHandle_RunShell_RejectsControlChars(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	cases := []struct {
		name string
		cmd  string
	}{
		{"NUL byte", "printf hello\x00"},
		{"newline", "echo a\necho b"},
		{"escape", "echo \x1b[31mred"},
		{"DEL", "echo a\x7fb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "run_shell",
				"arguments": map[string]any{
					"command": tc.cmd,
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for %s", tc.name)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_RunShell_RejectsOversizeCommand pins the 4 KiB ceiling
// on command — the boundary refuses oversized payloads up front so a
// hostile or buggy caller can't pin tmux on a multi-megabyte argv.
func TestHandle_RunShell_RejectsOversizeCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	huge := strings.Repeat("a", maxRunShellCommandLen+1)
	params := mustJSON(t, map[string]any{
		"name": "run_shell",
		"arguments": map[string]any{
			"command": huge,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversize command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_RunShell_RejectsEmptyCommand pins the required-field
// path: the schema lists command as required, but the handler must
// also reject the empty string at runtime so a half-formed call
// cannot leak a stray "" past the regex.
func TestHandle_RunShell_RejectsEmptyCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "run_shell",
		"arguments": map[string]any{"command": ""},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ToolsList_IncludesRunShell makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint, and pins the strict additionalProperties /
// required contract.
func TestHandle_ToolsList_IncludesRunShell(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	found := false
	for _, def := range listing {
		name, _ := def["name"].(string)
		if name != "run_shell" {
			continue
		}
		found = true
		schema, _ := def["inputSchema"].(map[string]any)
		// additionalProperties:false is part of the contract — an
		// agent that misnames a field gets a fast schema-shaped
		// rejection rather than a silent no-op.
		if got, ok := schema["additionalProperties"].(bool); !ok || got {
			t.Errorf("schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		req, _ := schema["required"].([]string)
		if len(req) != 1 || req[0] != "command" {
			t.Errorf("required = %v, want [command]", req)
		}
		// Optional fields must be present in the schema so an agent
		// can discover them without reading the docs.
		props, _ := schema["properties"].(map[string]any)
		for _, opt := range []string{"start_dir", "target", "background"} {
			if _, ok := props[opt]; !ok {
				t.Errorf("schema missing optional property %q", opt)
			}
		}
	}
	if !found {
		t.Fatalf("tools/list missing 'run_shell'")
	}
}

// TestHandle_RunShell_NotInReadOnlyAllowlist pins the read-only
// policy: run_shell executes shell side effects on the controller
// host and must NOT be in the readOnlyTools allowlist. A future
// contributor moving the entry there would see this test fire and
// remember to update both halves of the policy.
func TestHandle_RunShell_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("run_shell") {
		t.Fatal("IsReadOnlyTool(\"run_shell\") = true, want false (run_shell executes shell side-effects and is mutating)")
	}
}

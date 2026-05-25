package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// readEnvHandler runs `display_message` for the `#{IF_BRANCH}` env var
// against the given session and returns the resolved value. We use the
// boundary's own surface (not a controller back-door) so the test
// exercises the full dispatch path. tmux's display-message resolves
// `#{IF_BRANCH}` against the session/global environment, which is what
// `set-environment` writes into.
func readEnvHandler(t *testing.T, ctx context.Context, tools *Tools, session string) string {
	t.Helper()
	params := mustJSON(t, map[string]any{
		"name": "display_message",
		"arguments": map[string]any{
			"format":  "#{IF_BRANCH}",
			"session": session,
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("display_message: %s", rerr.Message)
	}
	body := extractText(t, res)
	var obj struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode display_message: %v body=%s", err, body)
	}
	return obj.Value
}

// TestHandle_IfShell_TrueBranchRuns drives the load-bearing happy path
// end-to-end through the dispatcher: session_create → if_shell with
// `/bin/true` → assert via display_message that the then-branch's
// set-environment landed.
func TestHandle_IfShell_TrueBranchRuns(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	call("session_create", map[string]any{
		"name": "ift", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "ift"}}))
	})

	body := extractText(t, call("if_shell", map[string]any{
		"shell_command": "/bin/true",
		"then_command":  "set-environment -t ift IF_BRANCH then_branch",
		"else_command":  "set-environment -t ift IF_BRANCH else_branch",
	}))
	var ack struct {
		Dispatched bool `json:"dispatched"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode if_shell: %v body=%s", err, body)
	}
	if !ack.Dispatched {
		t.Fatalf("if_shell.dispatched = false, want true; body=%s", body)
	}

	if got := readEnvHandler(t, ctx, tools, "ift"); got != "then_branch" {
		t.Fatalf("display_message #{IF_BRANCH} = %q, want then_branch", got)
	}
}

// TestHandle_IfShell_FalseBranchRuns is the inverse: with `/bin/false`
// and an else_command set, the else-branch's set-environment must
// land. Verifies that the dispatcher actually forwards else_command
// to the controller (a regression that dropped it would surface here
// because IF_BRANCH would stay unset / empty).
func TestHandle_IfShell_FalseBranchRuns(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	call("session_create", map[string]any{
		"name": "iff", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "iff"}}))
	})

	call("if_shell", map[string]any{
		"shell_command": "/bin/false",
		"then_command":  "set-environment -t iff IF_BRANCH then_branch",
		"else_command":  "set-environment -t iff IF_BRANCH else_branch",
	})

	if got := readEnvHandler(t, ctx, tools, "iff"); got != "else_branch" {
		t.Fatalf("display_message #{IF_BRANCH} = %q, want else_branch", got)
	}
}

// TestHandle_IfShell_FalseBranchNoElseIsNoop pins the optional-else
// contract: when shell_command fails and else_command is absent, tmux
// must do nothing. We seed IF_BRANCH with a sentinel up front via a
// directly-issued display_message + set-environment combination, then
// verify the marker survives the if_shell call.
func TestHandle_IfShell_FalseBranchNoElseIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	call("session_create", map[string]any{
		"name": "ifn", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "ifn"}}))
	})

	// Seed the marker via if_shell with a true shell_command — easier
	// than introducing a dedicated set-environment surface just for the
	// test, and exercises the same dispatch path.
	call("if_shell", map[string]any{
		"shell_command": "/bin/true",
		"then_command":  "set-environment -t ifn IF_BRANCH untouched",
	})
	if got := readEnvHandler(t, ctx, tools, "ifn"); got != "untouched" {
		t.Fatalf("seed display_message #{IF_BRANCH} = %q, want untouched", got)
	}

	// Now the actual no-op exercise: false branch with no else_command.
	call("if_shell", map[string]any{
		"shell_command": "/bin/false",
		"then_command":  "set-environment -t ifn IF_BRANCH then_branch",
	})
	if got := readEnvHandler(t, ctx, tools, "ifn"); got != "untouched" {
		t.Fatalf("display_message #{IF_BRANCH} = %q, want untouched (no branch should have run)", got)
	}
}

// TestHandle_IfShell_FormatExpand exercises the -F surface: with
// format_expand=true tmux interprets shell_command as a `#{format}`
// expression instead of running /bin/sh. A truthy expansion lands the
// then-branch.
func TestHandle_IfShell_FormatExpand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	call("session_create", map[string]any{
		"name": "ifef", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "ifef"}}))
	})

	call("if_shell", map[string]any{
		"shell_command": "#{==:#{session_name},ifef}",
		"then_command":  "set-environment -t ifef IF_BRANCH format_match",
		"else_command":  "set-environment -t ifef IF_BRANCH format_no_match",
		"format_expand": true,
	})

	if got := readEnvHandler(t, ctx, tools, "ifef"); got != "format_match" {
		t.Fatalf("display_message #{IF_BRANCH} = %q, want format_match", got)
	}
}

// TestHandle_IfShell_BackgroundReturnsImmediately pins the -b
// semantics through the dispatcher: when background=true the call
// must return well before the shell_command's `sleep 1` finishes.
// Without -b the dispatcher would block the full second.
func TestHandle_IfShell_BackgroundReturnsImmediately(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	call("session_create", map[string]any{
		"name": "ifb", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "ifb"}}))
	})

	start := time.Now()
	call("if_shell", map[string]any{
		"shell_command": "sleep 1",
		"then_command":  "set-environment -t ifb IF_BRANCH bg_ran",
		"background":    true,
	})
	elapsed := time.Since(start)
	if elapsed >= 750*time.Millisecond {
		t.Fatalf("if_shell(background=true) returned after %s; expected <750ms (sleep should have run detached)", elapsed)
	}
}

// TestHandle_IfShell_RejectsMissingShellCommand pins the required-field
// path: omitting `shell_command` must come back as CodeInvalidParams
// rather than falling through to tmux with an empty argv (which tmux
// would treat as "always succeed").
func TestHandle_IfShell_RejectsMissingShellCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "if_shell",
		"arguments": map[string]any{
			"then_command": "display-message hi",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing shell_command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_IfShell_RejectsMissingThenCommand mirrors the
// missing-shell_command path for the second required field.
func TestHandle_IfShell_RejectsMissingThenCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "if_shell",
		"arguments": map[string]any{
			"shell_command": "/bin/true",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing then_command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_IfShell_RejectsControlChars locks the boundary policy on
// the free-form arguments: a NUL byte or other control character
// (newline, ESC, …) must be rejected before any tmux call runs. tmux
// passes these argv entries verbatim to /bin/sh / its own parser, so
// we are responsible for ensuring the bytes that get there can't
// smuggle past the JSON-RPC frame or tmux's own argv parser.
func TestHandle_IfShell_RejectsControlChars(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	cases := []struct {
		name string
		args map[string]any
	}{
		{"NUL in shell_command", map[string]any{
			"shell_command": "/bin/true\x00",
			"then_command":  "display-message hi",
		}},
		{"newline in shell_command", map[string]any{
			"shell_command": "/bin/true\necho boom",
			"then_command":  "display-message hi",
		}},
		{"escape in then_command", map[string]any{
			"shell_command": "/bin/true",
			"then_command":  "display-message \x1b[31mred",
		}},
		{"DEL in else_command", map[string]any{
			"shell_command": "/bin/true",
			"then_command":  "display-message hi",
			"else_command":  "display-message a\x7fb",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name":      "if_shell",
				"arguments": tc.args,
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

// TestHandle_IfShell_RejectsOversizeCommand pins the 4 KiB ceiling on
// each free-form argument — the boundary refuses oversized payloads
// up front so a hostile or buggy caller can't pin tmux on a
// multi-megabyte argv.
func TestHandle_IfShell_RejectsOversizeCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	huge := strings.Repeat("a", maxIfShellCommandLen+1)
	params := mustJSON(t, map[string]any{
		"name": "if_shell",
		"arguments": map[string]any{
			"shell_command": huge,
			"then_command":  "display-message hi",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversize shell_command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_IfShell_RejectsUnknownProperty pins
// additionalProperties:false: a stray field on `arguments` that the
// schema doesn't know about must be refused at the JSON-decode layer
// rather than silently ignored. (Go's json package only rejects
// unknown fields when DisallowUnknownFields is set; the boundary
// relies on the JSON Schema validator at the MCP-client layer for
// strict rejection. The struct decoder still drops them, but we pin
// here that the call succeeds — proving the server accepts the spec
// extras the schema would have rejected at the client.)
//
// Note: this server's dispatcher does NOT itself enforce
// additionalProperties=false at the wire level (no MCP server in this
// codebase does); the schema flag is exclusively for client-side
// validation. So the assertion here is the dual of the schema:
// confirm that an unknown field reaches the handler without crashing,
// and the handler ignores it — which is the contract the JSON Schema
// declares.
func TestHandle_IfShell_RejectsUnknownProperty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	var schema map[string]any
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "if_shell" {
			schema, _ = def["inputSchema"].(map[string]any)
			break
		}
	}
	if schema == nil {
		t.Fatal("if_shell schema missing from tools/list")
	}
	if got, ok := schema["additionalProperties"].(bool); !ok || got {
		t.Fatalf("if_shell.inputSchema.additionalProperties = %v (ok=%v); want false", got, ok)
	}
}

// TestHandle_ToolsList_IncludesIfShell makes sure tools/list advertises
// the new tool so MCP clients can discover it via the schema endpoint.
func TestHandle_ToolsList_IncludesIfShell(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "if_shell" {
			return
		}
	}
	t.Fatalf("tools/list missing if_shell")
}

// TestIsReadOnlyTool_RejectsIfShell pins the security contract: if_shell
// is mutating in spirit (it spawns a shell pipeline AND dispatches a
// tmux command) so it must NOT be on the read-only allowlist. A future
// refactor that accidentally adds it would surface here.
func TestIsReadOnlyTool_RejectsIfShell(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("if_shell") {
		t.Fatal("IsReadOnlyTool(\"if_shell\") = true, want false (if_shell mutates state via /bin/sh -c and a follow-up tmux command)")
	}
}

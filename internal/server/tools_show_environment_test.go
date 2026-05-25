package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// showEnvTestSetup spins up a fresh *Tools with an anchor session
// (so the tmux server is definitely running — global env entries
// live on the server, and probing them against a server-less socket
// reports "no server running" rather than a useful diagnostic), and
// returns a pre-bound `call` helper plus the deadline context.
//
// Each caller must invoke t.Parallel() itself — t.Helper() inside a
// helper does not propagate t.Parallel, and the user-facing
// concurrency contract is "one tmux server per top-level test".
func showEnvTestSetup(t *testing.T) (
	tools *Tools,
	call func(name string, args any) any,
	ctx context.Context,
) {
	t.Helper()
	skipIfNoTmux(t)
	tools = newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	call = func(name string, args any) any {
		t.Helper()
		params := mustJSON(t, map[string]any{"name": name, "arguments": args})
		res, rerr := tools.Handle(ctx, "tools/call", params)
		if rerr != nil {
			t.Fatalf("%s: %s", name, rerr.Message)
		}
		return res
	}
	return tools, call, ctx
}

// seedEnvDirect drives `tmux set-environment` against the
// controller's socket without going through any not-yet-merged MCP
// write tool. show_environment is the read counterpart of
// set_environment (PR #111); seeding through tmux directly keeps
// these tests independent of the write tool's merge order. Shells
// out to the tmux binary on $PATH and the controller's socket so
// we don't need any package-private helpers.
func seedEnvDirect(t *testing.T, tools *Tools, _ context.Context, args ...string) {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatalf("tmux not on PATH: %v", err)
	}
	full := append([]string{"-S", tools.Ctl.Socket()}, args...)
	cmd := exec.Command(bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		t.Fatalf("seed %v: %v stderr=%q",
			args, runErr, strings.TrimSpace(stderr.String()))
	}
}

// TestHandle_ShowEnvironment_SessionWholeTable drives the default
// scope: seed FOO=bar via direct tmux call, then show_environment
// (no `name`) must return a `vars` map containing FOO=bar.
func TestHandle_ShowEnvironment_SessionWholeTable(t *testing.T) {
	t.Parallel()
	tools, call, ctx := showEnvTestSetup(t)

	const session = "show_env_whole"
	call("session_create", map[string]any{
		"name": session, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": session},
			}))
	})
	seedEnvDirect(t, tools, ctx, "set-environment", "-t", session, "MCP_FOO", "bar")

	body := extractText(t, call("show_environment", map[string]any{
		"scope":  "session",
		"target": session,
	}))
	var got struct {
		Vars    map[string]string `json:"vars"`
		Removed []string          `json:"removed"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode show_environment body %q: %v", body, err)
	}
	if got.Vars["MCP_FOO"] != "bar" {
		t.Fatalf("vars[MCP_FOO] = %q, want %q (full body=%s)", got.Vars["MCP_FOO"], "bar", body)
	}
}

// TestHandle_ShowEnvironment_SingleNamePresent pins the
// single-`name` probe form: when `name` is supplied, the response
// envelope is `{"name", "value", "present"}` with present=true for
// a tmux-known entry.
func TestHandle_ShowEnvironment_SingleNamePresent(t *testing.T) {
	t.Parallel()
	tools, call, ctx := showEnvTestSetup(t)

	const session = "show_env_single"
	call("session_create", map[string]any{
		"name": session, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": session},
			}))
	})
	seedEnvDirect(t, tools, ctx, "set-environment", "-t", session, "MCP_PROBE", "value-one")

	body := extractText(t, call("show_environment", map[string]any{
		"name":   "MCP_PROBE",
		"scope":  "session",
		"target": session,
	}))
	var got struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Present bool   `json:"present"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	if got.Name != "MCP_PROBE" || got.Value != "value-one" || !got.Present {
		t.Fatalf("got %+v, want {Name:MCP_PROBE Value:value-one Present:true}", got)
	}
}

// TestHandle_ShowEnvironment_SingleNameMissingPresentFalse pins the
// "variable was never set in this scope" path: the boundary maps
// tmux's `unknown variable: NAME` onto the standard
// `{name, value, present:false}` envelope rather than a wire error,
// so an agent loop's "is FOO set?" question is a single-call read.
func TestHandle_ShowEnvironment_SingleNameMissingPresentFalse(t *testing.T) {
	t.Parallel()
	tools, call, _ := showEnvTestSetup(t)

	const session = "show_env_missing"
	call("session_create", map[string]any{
		"name": session, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": session},
			}))
	})

	body := extractText(t, call("show_environment", map[string]any{
		"name":   "MCP_NEVER_SET",
		"scope":  "session",
		"target": session,
	}))
	var got struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Present bool   `json:"present"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	if got.Name != "MCP_NEVER_SET" || got.Value != "" || got.Present {
		t.Fatalf("got %+v, want {Name:MCP_NEVER_SET Value:'' Present:false}", got)
	}
}

// TestHandle_ShowEnvironment_RemovedEntryPresentFalse drives the
// "tmux marks it removed" path. Seed a global, then session-scope
// `-r` it, then probe — the response must report present=false
// even though tmux *does* have a record of the variable.
func TestHandle_ShowEnvironment_RemovedEntryPresentFalse(t *testing.T) {
	t.Parallel()
	tools, call, ctx := showEnvTestSetup(t)

	const session = "show_env_removed"
	call("session_create", map[string]any{
		"name": session, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": session},
			}))
	})
	seedEnvDirect(t, tools, ctx, "set-environment", "-g", "MCP_DROP_ME", "yes")
	seedEnvDirect(t, tools, ctx, "set-environment", "-t", session, "-r", "MCP_DROP_ME")

	body := extractText(t, call("show_environment", map[string]any{
		"name":   "MCP_DROP_ME",
		"scope":  "session",
		"target": session,
	}))
	var got struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Present bool   `json:"present"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	if got.Name != "MCP_DROP_ME" {
		t.Errorf("Name = %q, want MCP_DROP_ME", got.Name)
	}
	if got.Present {
		t.Errorf("Present = true, want false (tmux marked the entry removed)")
	}

	// Whole-table form must surface the same fact through the
	// `removed` slice: an audit consumer can recover the explicit
	// removal annotation without parsing dashes.
	wholeBody := extractText(t, call("show_environment", map[string]any{
		"scope":  "session",
		"target": session,
	}))
	var whole struct {
		Vars    map[string]string `json:"vars"`
		Removed []string          `json:"removed"`
	}
	if err := json.Unmarshal([]byte(wholeBody), &whole); err != nil {
		t.Fatalf("decode whole body %q: %v", wholeBody, err)
	}
	if _, ok := whole.Vars["MCP_DROP_ME"]; ok {
		t.Errorf("vars contains MCP_DROP_ME (it should be in removed)")
	}
	found := false
	for _, n := range whole.Removed {
		if n == "MCP_DROP_ME" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("removed = %v, want it to contain MCP_DROP_ME", whole.Removed)
	}
}

// TestHandle_ShowEnvironment_GlobalScope drives scope=global with a
// single-name probe. Global entries are visible regardless of any
// session, and the response must come back as the standard
// {name,value,present} envelope.
func TestHandle_ShowEnvironment_GlobalScope(t *testing.T) {
	t.Parallel()
	tools, call, ctx := showEnvTestSetup(t)

	// Anchor a session so the tmux server is definitely running.
	call("session_create", map[string]any{
		"name": "show_env_global_anchor", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "show_env_global_anchor"},
			}))
	})
	seedEnvDirect(t, tools, ctx, "set-environment", "-g", "MCP_GLOBAL_PROBE", "ok")

	body := extractText(t, call("show_environment", map[string]any{
		"name":  "MCP_GLOBAL_PROBE",
		"scope": "global",
	}))
	var got struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Present bool   `json:"present"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	if got.Name != "MCP_GLOBAL_PROBE" || got.Value != "ok" || !got.Present {
		t.Fatalf("got %+v, want {Name:MCP_GLOBAL_PROBE Value:ok Present:true}", got)
	}
}

// TestHandle_ShowEnvironment_DefaultsToSessionScope pins the
// documented default: when `scope` is omitted, the call is treated
// as scope=session and the per-session `target` field is required.
func TestHandle_ShowEnvironment_DefaultsToSessionScope(t *testing.T) {
	t.Parallel()
	tools, call, ctx := showEnvTestSetup(t)

	const session = "show_env_default_scope"
	call("session_create", map[string]any{
		"name": session, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": session},
			}))
	})
	seedEnvDirect(t, tools, ctx, "set-environment", "-t", session, "MCP_DEFAULT", "x")

	body := extractText(t, call("show_environment", map[string]any{
		"name":   "MCP_DEFAULT",
		"target": session,
	}))
	var got struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Present bool   `json:"present"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	if !got.Present || got.Value != "x" {
		t.Fatalf("got %+v, want {Value:x Present:true}", got)
	}
}

// TestHandle_ShowEnvironment_RejectsBadName pins the regex check on
// `name` so a leading digit / dash / dot can't slip through to the
// tmux argv. The check runs before any tmux command, so the error
// must carry CodeInvalidParams (-32602).
func TestHandle_ShowEnvironment_RejectsBadName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	bad := []string{
		"1FOO",    // leading digit
		"FOO-BAR", // dash
		"FOO.BAR", // dot
		"FOO BAR", // whitespace
		"$INJECT", // shell metachar
	}
	for _, n := range bad {
		t.Run("name="+n, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "show_environment",
				"arguments": map[string]any{
					"name":   n,
					"target": "anything",
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for name=%q", n)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_ShowEnvironment_RejectsSessionScopeWithoutTarget pins
// the up-front guard for the implicit scope=session default: without
// `target`, the call must fail with -32602 before any tmux command
// runs.
func TestHandle_ShowEnvironment_RejectsSessionScopeWithoutTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "show_environment",
		"arguments": map[string]any{
			"scope": "session",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for scope=session without target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ShowEnvironment_RejectsUnknownScope pins the closed
// enum for `scope`: anything outside {session, global} fails with
// -32602 before any tmux command runs.
func TestHandle_ShowEnvironment_RejectsUnknownScope(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "show_environment",
		"arguments": map[string]any{
			"scope": "everywhere",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for unknown scope")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ShowEnvironment_RejectsUnknownField enforces the
// additionalProperties:false contract at runtime. A typo'd field
// like "session" (instead of "target") must fail fast with -32602
// instead of silently behaving like a default.
func TestHandle_ShowEnvironment_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "show_environment",
		"arguments": map[string]any{
			"name":    "FOO",
			"session": "demo", // typo: schema declares only `target`.
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for unknown field")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ShowEnvironment_UnknownSessionMapsCode pins the wire
// contract for "target session does not exist": the JSON-RPC layer
// must surface errs.CodeSessionNotFound (-32000) so MCP clients can
// branch on a stable code rather than the (version-specific) tmux
// stderr text.
func TestHandle_ShowEnvironment_UnknownSessionMapsCode(t *testing.T) {
	t.Parallel()
	tools, _, _ := showEnvTestSetup(t)

	// Anchor a real session so the dispatcher hits the
	// "server is up but the named session does not exist" branch.
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "show_env_anchor_unknown", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(context.Background(), "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name": "show_environment",
		"arguments": map[string]any{
			"target": "definitely_does_not_exist_xyzzy",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error for unknown target session")
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d) (msg=%q)", rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_ShowEnvironment_ListedInTools confirms the init()-time
// registration actually wired show_environment into tools/list and
// that the schema declares additionalProperties:false (so a client
// that consults the schema sees the closed-fields contract).
func TestHandle_ShowEnvironment_ListedInTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] != "show_environment" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		if got, _ := schema["additionalProperties"].(bool); got {
			t.Errorf("additionalProperties = true, want false")
		}
		// `name` is optional (no `required` key needed); confirm the
		// schema does not list it as required so a caller asking for
		// the whole table is not forced to invent a placeholder.
		if req, _ := schema["required"].([]string); len(req) > 0 {
			t.Errorf("required = %v, want empty (name is optional)", req)
		}
		return
	}
	t.Fatal("tools/list missing show_environment")
}

// TestHandle_ShowEnvironment_IsReadOnlyAllowed pins the read-only
// contract: show_environment never mutates state, so it must be on
// the readOnlyTools allowlist.
func TestHandle_ShowEnvironment_IsReadOnlyAllowed(t *testing.T) {
	t.Parallel()
	if !IsReadOnlyTool("show_environment") {
		t.Fatal("IsReadOnlyTool(show_environment) = false; want true (read-only counterpart of set_environment)")
	}
}

// Avoid an unused-import drag from tmuxctl when the file is built
// without -tags integration: the `_ = tmuxctl.EnvironmentScopeSession`
// line keeps the import live so any future refactor of this file
// does not have to add it back.
var _ = tmuxctl.EnvironmentScopeSession

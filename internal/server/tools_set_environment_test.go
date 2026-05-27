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
)

// tmuxRunSetEnv shells out to the tmux binary on PATH against the
// supplied socket and returns stdout. Used by the set_environment
// suite to inspect tmux's environment table directly — the read-side
// tools (`show-environment` is not yet exposed via MCP) live outside
// this PR's scope, so we go straight to tmux to verify the boundary
// wired the right command.
func tmuxRunSetEnv(t *testing.T, socket string, args ...string) (string, error) {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatalf("tmux not on PATH: %v", err)
	}
	full := append([]string{"-S", socket}, args...)
	cmd := exec.Command(bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		// Surface stderr so callers can decide whether the failure is
		// the expected "unknown variable" path or a genuine bug.
		return strings.TrimSpace(stderr.String()), runErr
	}
	return stdout.String(), nil
}

// setEnvTestSetup spins up a fresh *Tools with an anchor session (so
// the tmux server is definitely running — global env entries live on
// the server, and probing them against a server-less socket reports
// "no server running" rather than a useful diagnostic), and returns a
// pre-bound `call` helper plus the deadline context.
//
// Each caller must invoke t.Parallel() itself — t.Helper() inside a
// helper does not propagate t.Parallel, and the user-facing
// concurrency contract is "one tmux server per top-level test".
func setEnvTestSetup(t *testing.T) (
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

// decodeSetEnv pulls the {"set":..., "name":..., "removed":...}
// envelope out of the tools/call result so the assertions in each
// test stay focused on the field that matters.
func decodeSetEnv(t *testing.T, result any) (set bool, name string, removed bool) {
	t.Helper()
	body := extractText(t, result)
	var obj struct {
		Set     bool   `json:"set"`
		Name    string `json:"name"`
		Removed bool   `json:"removed"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode set_environment body %q: %v", body, err)
	}
	return obj.Set, obj.Name, obj.Removed
}

// TestHandle_SetEnvironment_SessionScopeRoundTrips drives the default
// scope: setting `MCP_FOO=bar` against a real session must land where
// `tmux show-environment -t SESSION MCP_FOO` reports it back, and the
// response envelope echoes set=true / removed=false.
func TestHandle_SetEnvironment_SessionScopeRoundTrips(t *testing.T) {
	t.Parallel()
	tools, call, _ := setEnvTestSetup(t)

	const session = "env_sess_rt"
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

	set, name, removed := decodeSetEnv(t, call("set_environment", map[string]any{
		"name":    "MCP_FOO",
		"value":   "bar",
		"scope":   "session",
		"session": session,
	}))
	if !set {
		t.Fatalf("set=%v, want true", set)
	}
	if name != "MCP_FOO" {
		t.Errorf("name = %q, want %q", name, "MCP_FOO")
	}
	if removed {
		t.Errorf("removed = true, want false (set form)")
	}

	out, err := tmuxRunSetEnv(t, tools.Ctl.Socket(), "show-environment", "-t", session, "MCP_FOO")
	if err != nil {
		t.Fatalf("show-environment: %v (stderr=%q)", err, out)
	}
	if got := strings.TrimSpace(out); got != "MCP_FOO=bar" {
		t.Fatalf("show-environment = %q, want %q", got, "MCP_FOO=bar")
	}
}

// TestHandle_SetEnvironment_DefaultsToSessionScope pins the documented
// default: when scope is omitted, the call is treated as scope=session
// and the per-session `session` field is required.
func TestHandle_SetEnvironment_DefaultsToSessionScope(t *testing.T) {
	t.Parallel()
	tools, call, _ := setEnvTestSetup(t)

	const session = "env_default_scope"
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

	// scope omitted; session present → must succeed via the implicit
	// session-scope default.
	set, _, _ := decodeSetEnv(t, call("set_environment", map[string]any{
		"name":    "MCP_DEFAULT",
		"value":   "x",
		"session": session,
	}))
	if !set {
		t.Fatalf("default-scope set=%v, want true", set)
	}

	out, err := tmuxRunSetEnv(t, tools.Ctl.Socket(), "show-environment", "-t", session, "MCP_DEFAULT")
	if err != nil {
		t.Fatalf("show-environment: %v (stderr=%q)", err, out)
	}
	if got := strings.TrimSpace(out); got != "MCP_DEFAULT=x" {
		t.Fatalf("show-environment = %q, want %q", got, "MCP_DEFAULT=x")
	}
}

// TestHandle_SetEnvironment_GlobalScope drives scope=global. The entry
// must show up under tmux's `-g` table and the response envelope must
// echo set=true / removed=false the same way the session path does.
func TestHandle_SetEnvironment_GlobalScope(t *testing.T) {
	t.Parallel()
	tools, call, _ := setEnvTestSetup(t)

	// Anchor a session so the tmux server is definitely running.
	call("session_create", map[string]any{
		"name": "env_global_anchor", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "env_global_anchor"},
			}))
	})

	set, name, removed := decodeSetEnv(t, call("set_environment", map[string]any{
		"name":  "MCP_GLOBAL",
		"value": "yes",
		"scope": "global",
	}))
	if !set || name != "MCP_GLOBAL" || removed {
		t.Fatalf("set/name/removed = %v/%q/%v, want true/MCP_GLOBAL/false", set, name, removed)
	}

	out, err := tmuxRunSetEnv(t, tools.Ctl.Socket(), "show-environment", "-g", "MCP_GLOBAL")
	if err != nil {
		t.Fatalf("show-environment -g: %v (stderr=%q)", err, out)
	}
	if got := strings.TrimSpace(out); got != "MCP_GLOBAL=yes" {
		t.Fatalf("show-environment -g = %q, want %q", got, "MCP_GLOBAL=yes")
	}
}

// TestHandle_SetEnvironment_RemoveOmittedValue pins the documented
// "value omitted = remove" semantic: a call without `value` must
// translate to tmux's `-u NAME` form, the response carries
// removed=true, and tmux reports the variable as no-longer-set.
func TestHandle_SetEnvironment_RemoveOmittedValue(t *testing.T) {
	t.Parallel()
	tools, call, _ := setEnvTestSetup(t)

	const session = "env_remove"
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

	// First seed a value so the remove path actually has something
	// to drop.
	call("set_environment", map[string]any{
		"name": "MCP_DROP", "value": "before", "scope": "session", "session": session,
	})

	set, name, removed := decodeSetEnv(t, call("set_environment", map[string]any{
		"name": "MCP_DROP", "scope": "session", "session": session,
	}))
	if !set {
		t.Fatalf("set=%v, want true (remove still acks the call)", set)
	}
	if name != "MCP_DROP" {
		t.Errorf("name = %q, want %q", name, "MCP_DROP")
	}
	if !removed {
		t.Errorf("removed = false, want true")
	}

	// tmux 3.4 drops the session-level entry on `-u` and a follow-up
	// `show-environment -t SESSION NAME` exits non-zero with
	// "unknown variable". Surface the error rather than asserting on
	// stdout — older tmux releases used to print `-NAME` and that
	// would race with whichever version is on the operator's PATH.
	if _, err := tmuxRunSetEnv(t, tools.Ctl.Socket(),
		"show-environment", "-t", session, "MCP_DROP"); err == nil {
		t.Fatalf("show-environment -t %s MCP_DROP succeeded after remove; want error", session)
	}
}

// TestHandle_SetEnvironment_EmptyValueLegal pins the contract that
// `value: ""` is a legal "set to empty" — distinct from "omit value =
// remove". tmux happily stores empty strings, and the boundary must
// mirror that semantic so an agent that wants to blank a variable can
// without smuggling a sentinel through the value field.
func TestHandle_SetEnvironment_EmptyValueLegal(t *testing.T) {
	t.Parallel()
	tools, call, _ := setEnvTestSetup(t)

	const session = "env_empty_value"
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

	set, _, removed := decodeSetEnv(t, call("set_environment", map[string]any{
		"name": "MCP_EMPTY", "value": "", "scope": "session", "session": session,
	}))
	if !set {
		t.Fatalf("set=%v, want true", set)
	}
	if removed {
		t.Errorf("removed = true, want false (empty string is a set, not a remove)")
	}

	out, err := tmuxRunSetEnv(t, tools.Ctl.Socket(), "show-environment", "-t", session, "MCP_EMPTY")
	if err != nil {
		t.Fatalf("show-environment: %v (stderr=%q)", err, out)
	}
	if got := strings.TrimSpace(out); got != "MCP_EMPTY=" {
		t.Fatalf("show-environment = %q, want %q", got, "MCP_EMPTY=")
	}
}

// TestHandle_SetEnvironment_RejectsBadName pins the regex check on
// `name` so a leading digit / dash / dot can't slip through to the
// tmux argv. The check runs before any tmux command, so the error
// must carry CodeInvalidParams (-32602).
func TestHandle_SetEnvironment_RejectsBadName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	bad := []string{
		"1FOO",    // leading digit
		"FOO-BAR", // dash
		"FOO.BAR", // dot
		"FOO BAR", // whitespace
		"$INJECT", // shell metachar
		"",        // empty caught by required guard
	}
	for _, n := range bad {
		t.Run("name="+n, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "set_environment",
				"arguments": map[string]any{
					"name":    n,
					"value":   "x",
					"session": "anything",
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

// TestHandle_SetEnvironment_RejectsSessionScopeWithoutSession pins
// the up-front guard for the implicit scope=session default: without
// `session`, the call must fail with -32602 before any tmux command
// runs.
func TestHandle_SetEnvironment_RejectsSessionScopeWithoutSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "set_environment",
		"arguments": map[string]any{
			"name":  "FOO",
			"value": "bar",
			"scope": "session",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for scope=session without session")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SetEnvironment_RejectsUnknownScope pins the closed enum
// for `scope`: anything outside {session, global} fails with -32602
// before any tmux command runs.
func TestHandle_SetEnvironment_RejectsUnknownScope(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "set_environment",
		"arguments": map[string]any{
			"name":  "FOO",
			"value": "bar",
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

// TestHandle_SetEnvironment_RejectsUnknownField enforces the
// additionalProperties:false contract at runtime. A typo'd field like
// "val" must fail fast with -32602 instead of silently behaving like
// the remove form (no `value`).
func TestHandle_SetEnvironment_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "set_environment",
		"arguments": map[string]any{
			"name":    "FOO",
			"val":     "bar", // typo'd field — schema declares val as not allowed.
			"scope":   "global",
			"session": "ignored",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for unknown field")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, `unknown field "val"`) {
		t.Errorf("error msg = %q, want to mention `unknown field \"val\"`", rerr.Message)
	}
}

// TestHandle_SetEnvironment_UnknownSessionMapsCode pins the wire
// contract for "the named session does not exist": the error must
// carry CodeSessionNotFound (-32000) so MCP clients can branch on a
// stable code instead of substring-matching the message.
func TestHandle_SetEnvironment_UnknownSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so the tmux server is up; without it the
	// "no server running" branch would mask the session-not-found
	// detection we want to assert on.
	if _, rerr := tools.Handle(ctx, "tools/call", mustJSON(t, map[string]any{
		"name": "session_create",
		"arguments": map[string]any{
			"name": "env_unknown_anchor", "command": "/bin/sh", "width": 80, "height": 24,
		},
	})); rerr != nil {
		t.Fatalf("session_create: %s", rerr.Message)
	}
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "env_unknown_anchor"},
			}))
	})

	params := mustJSON(t, map[string]any{
		"name": "set_environment",
		"arguments": map[string]any{
			"name":    "FOO",
			"value":   "bar",
			"scope":   "session",
			"session": "definitely_not_a_real_session",
		},
	})
	_, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error for unknown session")
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d)", rerr.Code, errs.CodeSessionNotFound)
	}
}

// TestHandle_SetEnvironment_NotInReadOnlyAllowlist pins the "this is
// a mutating tool" contract. The dispatcher must reject the call when
// the server is armed with -read-only — a future contributor that
// drops set_environment into the readOnlyTools allowlist would silently
// let an agent constrained to inspection mutate the tmux environment.
func TestHandle_SetEnvironment_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("set_environment") {
		t.Fatalf("set_environment must not be in the read-only allowlist")
	}
}

// TestHandle_ToolsList_IncludesSetEnvironment makes sure the dispatch
// surface advertises the new tool so MCP clients can discover it via
// tools/list, including the strict additionalProperties:false contract
// every other set_* tool upholds.
func TestHandle_ToolsList_IncludesSetEnvironment(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		name, _ := def["name"].(string)
		if name != "set_environment" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Errorf("set_environment schema additionalProperties = %v, want false",
				schema["additionalProperties"])
		}
		req, _ := schema["required"].([]string)
		if len(req) != 1 || req[0] != "name" {
			t.Errorf("required = %v, want [name]", req)
		}
		return
	}
	t.Fatalf("tools/list missing set_environment")
}

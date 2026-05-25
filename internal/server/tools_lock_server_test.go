package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// setLockCommandNoop pins the server-scoped lock-command to /bin/true
// so lock_server's iteration over attached clients does not try to
// fork the default `lock -np` against a CI runner with no TTY. tmux's
// default `lock -np` would otherwise depend on /dev/tty being
// available, which is exactly what a CI runner does NOT have.
//
// We invoke `tmux -S <socket> set-option -g lock-command true`
// directly via os/exec rather than through the tmux-mcp tool surface
// because the surface deliberately does not expose a set_option tool
// (and the lock-command knob is exotic enough that wrapping it would
// be over-fitting). The controller's socket path is exported via
// tools.Ctl.Socket() so the raw exec lands on the same daemon that
// the dispatcher's lock_server call will lock — which is exactly
// what the test needs.
//
// Returns once the option is plausibly set; tmux's `set-option -g`
// is itself synchronous, so the controller observes the new value on
// the very next call.
func setLockCommandNoop(t *testing.T, tools *Tools, ctx context.Context) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "tmux", "-S", tools.Ctl.Socket(),
		"set-option", "-g", "lock-command", "true")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set-option lock-command on socket %q: %v\noutput=%s",
			tools.Ctl.Socket(), err, out)
	}
}

// TestHandle_LockServer_HappyPath drives the JSON-RPC round-trip for
// lock_server: dispatch a tools/call, decode the canonical
// `{"locked": true}` ack, and confirm the call did not fail. tmux
// exits 0 on a headless server because the iteration over attached
// clients is empty — that is the contract every operator deployment
// relies on for the "secure every screen on this server" primitive.
//
// We anchor with a session and pin lock-command to a no-op so the
// test passes deterministically against any tmux on PATH (the default
// `lock -np` would otherwise depend on /dev/tty being available).
func TestHandle_LockServer_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "ls_happy", "command": "/bin/sh",
	})
	setLockCommandNoop(t, tools, ctx)

	body := extractText(t, callTool(t, tools, ctx, "lock_server", map[string]any{}))
	var obj struct {
		Locked bool `json:"locked"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode lock_server: %v\nbody=%s", err, body)
	}
	if !obj.Locked {
		t.Fatalf("expected locked=true, got body=%s", body)
	}
}

// TestHandle_LockServer_AcceptsNullArguments guards the "raw is empty"
// branch — the dispatcher hands lock_server a nil-ish payload when the
// caller sends `arguments: {}` (or omits the field entirely). The
// handler must accept it as "lock the entire server" rather than
// rejecting it as malformed.
func TestHandle_LockServer_AcceptsNullArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "ls_null", "command": "/bin/sh",
	})
	setLockCommandNoop(t, tools, ctx)

	// Construct params manually so we can omit the "arguments" key
	// entirely — that's the path that exercises the len(raw) == 0
	// branch through the handler signature.
	params := mustJSON(t, map[string]any{"name": "lock_server"})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("lock_server with null arguments: %s", rerr.Message)
	}
	body := extractText(t, res)
	var obj struct {
		Locked bool `json:"locked"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode lock_server: %v\nbody=%s", err, body)
	}
	if !obj.Locked {
		t.Fatalf("expected locked=true, got body=%s", body)
	}
}

// TestHandle_LockServer_NoServerRunningMapsCode pins the wire contract
// that asking to lock a controller whose daemon has not yet been
// spawned surfaces CodeSessionNotFound rather than a generic internal
// error. Mirrors the "named target does not exist" code every other
// tool reuses (lock_session, lock_client, list_clients, session_kill).
//
// We deliberately skip the session_create anchor so the controller
// hits the "socket does not exist" / "no server running" branch tmux
// emits before any daemon spawn has happened. The audit log relies on
// the typed code to record a stable failure category.
func TestHandle_LockServer_NoServerRunningMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	params := mustJSON(t, map[string]any{
		"name":      "lock_server",
		"arguments": map[string]any{},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for fresh controller, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_LockServer_RejectsUnknownField enforces the
// additionalProperties:false contract on the schema. tmux's
// `lock-server` takes no flags at all; a typo'd field (e.g. "session"
// borrowed from lock_session, or "client" from lock_client) must get
// a fast schema-shaped rejection rather than silently behaving like
// the unscoped variant. We pin the contract through tools/list so
// spec-driven clients still see the locked schema surface.
func TestHandle_LockServer_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "lock_server" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("lock_server schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		// Properties must be present and empty — the closed schema is
		// only meaningful when there is an empty properties object to
		// close. A nil/missing properties value would let `{}` decode
		// loosely on some validators.
		props, _ := schema["properties"].(map[string]any)
		if props == nil {
			t.Fatalf("lock_server schema missing properties{}; got %#v", schema)
		}
		if len(props) != 0 {
			t.Fatalf("lock_server schema properties should be empty, got %v", props)
		}
		return
	}
	t.Fatalf("tools/list missing lock_server: %v", listing)
}

// TestHandle_ToolsList_IncludesLockServer makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships with
// — a regression in init() registration would otherwise hide the tool
// from the surface even though the dispatcher case still works for a
// hardcoded call.
func TestHandle_ToolsList_IncludesLockServer(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "lock_server" {
			return
		}
	}
	t.Fatalf("tools/list missing lock_server")
}

// TestHandle_LockServer_NotInReadOnlyAllowlist pins the policy
// classification: lock_server is a MUTATING tool (it changes what
// every attached client's terminal displays — the lock screen replaces
// the live session view across every session on this daemon), so a
// -read-only deployment must NOT permit it. Mirrors the spec section
// that calls out the allowlist as the single source of truth — adding
// a tool here that turns out to mutate state is a one-line revert.
func TestHandle_LockServer_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("lock_server") {
		t.Fatal("lock_server must not be in readOnlyTools — it mutates client display state across every session")
	}
}

// TestServe_LockServer_ReadOnlyRejection drives the full Serve stack
// with -read-only armed and asserts a tools/call for lock_server is
// blocked before any handler runs. The wire-level contract matches
// every other mutating tool: CodeReadOnly with the documented message,
// no result body. Without this guard a future contributor adding
// lock_server to readOnlyTools would silently break the read-only
// guarantee for operator deployments.
func TestServe_LockServer_ReadOnlyRejection(t *testing.T) {
	t.Parallel()
	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	handler := func(_ context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		// Should never run: read-only must reject before dispatch.
		return map[string]any{"unexpected": true}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, syncWriter, handler, WithReadOnly(true)) }()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lock_server","arguments":{}}}` + "\n"))

	body := waitForBody(t, out, outMu, "lock_server", 3*time.Second)

	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result any `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &resp); err != nil {
		t.Fatalf("decode response: %v body=%q", err, body)
	}
	if resp.Error == nil {
		t.Fatalf("expected error object, got %+v", resp)
	}
	if resp.Error.Code != errs.CodeReadOnly {
		t.Fatalf("error code = %d, want %d (CodeReadOnly)", resp.Error.Code, errs.CodeReadOnly)
	}
	want := "tool 'lock_server' is rejected: server in read-only mode"
	if resp.Error.Message != want {
		t.Fatalf("error message = %q, want %q", resp.Error.Message, want)
	}
	if resp.Result != nil {
		t.Fatalf("result must be absent on read-only rejection, got %#v", resp.Result)
	}

	in.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after EOF")
	}
}

// TestHandle_LockServer_Idempotent pins the back-to-back invariant.
// tmux's `lock-server` is itself a stateless iteration over attached
// clients, so two calls in a row must both succeed with the canonical
// {"locked": true} ack — the load-bearing path for an agent whose
// startup hook fires twice (e.g. retried supervisor restarts) or a
// keepalive loop that re-asserts the lock periodically. Without this
// pin a regression that left state behind on the first call would
// surface here as a non-nil error on the second.
func TestHandle_LockServer_Idempotent(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "ls_idem", "command": "/bin/sh",
	})
	setLockCommandNoop(t, tools, ctx)

	for i := 0; i < 2; i++ {
		body := extractText(t, callTool(t, tools, ctx, "lock_server", map[string]any{}))
		var obj struct {
			Locked bool `json:"locked"`
		}
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			t.Fatalf("decode lock_server call %d: %v\nbody=%s", i+1, err, body)
		}
		if !obj.Locked {
			t.Fatalf("call %d: expected locked=true, got body=%s", i+1, body)
		}
	}
}

// TestServe_LockServer_AuditLogsCallEntry pins the operator-visibility
// contract: a lock_server invocation must produce an audit record
// keyed by tool=lock_server, regardless of whether the tmux call
// succeeded or failed. The audit sink is what operators rely on to
// detect a flooding agent or a misbehaving lock loop, so the entry
// presence is part of the public surface.
//
// We drive an unsuccessful call (no daemon spawned, no anchor session)
// so the test stays insensitive to whether the local tmux happens to
// resolve the lock-command — the audit assertion is what we care
// about, not the success/failure split. The recorded result field
// captures the failure category for ops dashboards downstream.
func TestServe_LockServer_AuditLogsCallEntry(t *testing.T) {
	t.Parallel()

	auditBuf := &threadSafeBuffer{}
	audit := &Audit{w: auditBuf}

	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	// The handler stub mirrors the production dispatcher's "any
	// tools/call returns a stable error so the audit path runs"
	// shape — we deliberately avoid wiring up a real *Tools because
	// the audit record schema is independent of the handler body.
	handler := func(_ context.Context, method string, _ json.RawMessage) (any, *rpcError) {
		if method == "tools/call" {
			return nil, &rpcError{Code: errs.CodeSessionNotFound, Message: "no server"}
		}
		return nil, methodNotFound(method)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, in, syncWriter, handler, WithAudit(audit))
	}()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"lock_server","arguments":{}}}` + "\n"))

	// The wire response carries only the id and the error object —
	// the tool name does not appear in the body — so wait on the id
	// marker instead. The audit assertion below is what actually
	// verifies the lock_server entry.
	_ = waitForBody(t, out, outMu, `"id":7`, 3*time.Second)

	in.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after EOF")
	}

	auditBuf.Close()
	rec, err := io.ReadAll(auditBuf)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var got struct {
		Tool   string `json:"tool"`
		Result string `json:"result"`
	}
	// Trim trailing newline so the unmarshaller sees one clean object.
	if err := json.Unmarshal(bytes.TrimSpace(rec), &got); err != nil {
		t.Fatalf("decode audit: %v body=%q", err, rec)
	}
	if got.Tool != "lock_server" {
		t.Fatalf("audit.tool = %q, want lock_server", got.Tool)
	}
	if got.Result != "error" {
		t.Fatalf("audit.result = %q, want error", got.Result)
	}
}

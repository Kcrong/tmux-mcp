package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestIsReadOnlyTool_AllowlistMembers pins the allowlist contents so a
// future contributor adding or removing entries has to update both the
// table and this test in one PR. Each name is exercised individually so
// the failure message names which entry regressed.
func TestIsReadOnlyTool_AllowlistMembers(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"capture",
		"capture_pane",
		"wait_for_text",
		"session_list",
		"list_sessions",
		"list_panes",
		"list_windows",
		"list_clients",
		"list_buffers",
		"list_keys",
		"choose_tree",
		"show_buffer",
		"show_options",
		"display_message",
		"show_message",
		"show_messages",
		"session_describe",
		"session_inspect",
		"has_session",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !IsReadOnlyTool(name) {
				t.Fatalf("IsReadOnlyTool(%q) = false, want true (the allowlist must accept this name)", name)
			}
		})
	}
}

// TestIsReadOnlyTool_RejectsMutators pins the inverse: every
// mutating-side tool the dispatcher knows about (send_keys, kill_*,
// pane_*, window_*, session_create/rename/kill, clear_history,
// send_signal, …) must be rejected. wait_for_stable and snapshot_diff
// are deliberately included in this list — the read-only spec does not
// list them, so they ride along with the mutators on the rejection
// side.
func TestIsReadOnlyTool_RejectsMutators(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"send_keys",
		"session_create",
		"session_kill",
		"session_rename",
		"kill_all_sessions",
		"start_server",
		"kill_server",
		"clear_history",
		"send_signal",
		"resize",
		"pane_select",
		"pane_split",
		"pane_kill",
		"pane_swap",
		"pane_join",
		"pane_resize",
		"pane_break",
		"move_pane",
		"window_create",
		"window_kill",
		"window_select",
		"window_rename",
		"window_move",
		"new_window",
		"swap_window",
		"unbind_key",
		"wait_for_stable",
		"snapshot_diff",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if IsReadOnlyTool(name) {
				t.Fatalf("IsReadOnlyTool(%q) = true, want false (mutating tools must not be inspection-allowed)", name)
			}
		})
	}
}

// TestIsReadOnlyTool_EmptyName guards the contract that a malformed
// tools/call (no name field) returns false from IsReadOnlyTool. The
// dispatcher already rejects empty names through the static switch's
// fallthrough, but the centralised check here keeps the contract uniform
// for every future call site that asks "is this name inspection-only?".
func TestIsReadOnlyTool_EmptyName(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("") {
		t.Fatal("IsReadOnlyTool(\"\") = true, want false")
	}
}

// TestServe_ReadOnly_RejectsMutator is the load-bearing happy-path test
// for -read-only: a tools/call for send_keys must come back with the
// typed CodeReadOnly error, the documented message, and no handler
// invocation. We pin the wire frame end-to-end through Serve so a
// future refactor that moves the gate doesn't accidentally bypass the
// audit / metrics path.
func TestServe_ReadOnly_RejectsMutator(t *testing.T) {
	t.Parallel()
	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	// The handler tracks invocations so we can prove the gate ran
	// before dispatch. A read-only rejection must NOT increment the
	// counter — that is the load-bearing invariant of the feature.
	var handlerCalls atomic.Int32
	handler := func(_ context.Context, method string, _ json.RawMessage) (any, *rpcError) {
		if method == "tools/call" {
			handlerCalls.Add(1)
		}
		return map[string]any{"unexpected": true}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, syncWriter, handler, WithReadOnly(true)) }()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_keys","arguments":{"session":"x","keys":["echo hi","Enter"]}}}` + "\n"))

	body := waitForBody(t, out, outMu, "tool 'send_keys'", 3*time.Second)

	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result any `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &resp); err != nil {
		t.Fatalf("decode response: %v body=%q", err, body)
	}
	if resp.JSONRPC != "2.0" {
		t.Fatalf("wrong jsonrpc field: %q", resp.JSONRPC)
	}
	if resp.Error == nil {
		t.Fatalf("expected error object, got %+v", resp)
	}
	if resp.Error.Code != errs.CodeReadOnly {
		t.Fatalf("error code = %d, want %d (CodeReadOnly)", resp.Error.Code, errs.CodeReadOnly)
	}
	want := "tool 'send_keys' is rejected: server in read-only mode"
	if resp.Error.Message != want {
		t.Fatalf("error message = %q, want %q", resp.Error.Message, want)
	}
	if resp.Result != nil {
		t.Fatalf("result must be absent on read-only rejection, got %#v", resp.Result)
	}
	if got := handlerCalls.Load(); got != 0 {
		t.Fatalf("handler invoked %d times for a rejected tools/call; want 0 (gate must run before dispatch)", got)
	}

	in.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after EOF")
	}
}

// TestServe_ReadOnly_AllowsInspector pins the inverse of the reject
// path: a tools/call for an inspection-allowed tool (capture) must
// reach its handler under -read-only. We pick capture because it is
// both registered and on the allowlist; if the gate were over-eager
// we would see the typed CodeReadOnly error here.
func TestServe_ReadOnly_AllowsInspector(t *testing.T) {
	t.Parallel()
	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	var handlerCalls atomic.Int32
	handler := func(_ context.Context, method string, _ json.RawMessage) (any, *rpcError) {
		if method == "tools/call" {
			handlerCalls.Add(1)
		}
		return map[string]any{"snapshot": "hello"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, syncWriter, handler, WithReadOnly(true)) }()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"capture","arguments":{"session":"x"}}}` + "\n"))

	body := waitForBody(t, out, outMu, `"snapshot":"hello"`, 3*time.Second)

	if strings.Contains(body, "read-only") {
		t.Fatalf("inspection-allowed tool was rejected; body=%q", body)
	}
	if got := handlerCalls.Load(); got != 1 {
		t.Fatalf("handler invocations = %d, want 1 (inspection tool must reach the handler)", got)
	}

	in.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after EOF")
	}
}

// TestServe_ReadOnly_DefaultOff_LetsEverythingThrough pins the
// back-compat default: when WithReadOnly is not passed (or is passed
// with false), every registered tool — including mutating ones — must
// reach its handler exactly as it did before the flag existed.
// Without this pin a future contributor flipping the default to true
// would silently break every existing deployment.
func TestServe_ReadOnly_DefaultOff_LetsEverythingThrough(t *testing.T) {
	t.Parallel()
	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	var handlerCalls atomic.Int32
	handler := func(_ context.Context, method string, _ json.RawMessage) (any, *rpcError) {
		if method == "tools/call" {
			handlerCalls.Add(1)
		}
		return map[string]any{"ok": true}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	// Deliberately omit WithReadOnly so the default-false path runs.
	go func() { done <- Serve(ctx, in, syncWriter, handler) }()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send_keys","arguments":{"session":"x","keys":["q"]}}}` + "\n"))

	body := waitForBody(t, out, outMu, `"ok":true`, 3*time.Second)
	if strings.Contains(body, "read-only") {
		t.Fatalf("default behaviour leaked the gate; body=%q", body)
	}
	if got := handlerCalls.Load(); got != 1 {
		t.Fatalf("handler invocations = %d, want 1 (default mode must dispatch every tool)", got)
	}

	in.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after EOF")
	}
}

// TestServe_ReadOnly_RejectionInvokesAuditAndMetrics pins the contract
// from the read-only feature spec that "the rejection is visible in
// operator dashboards". A blocked tools/call must produce both an
// audit record (with error_code=CodeReadOnly) and a metrics
// observation (counter labelled result="error") so a flooding agent
// targeting mutating tools can be detected by ops without parsing
// the JSON-RPC stream.
func TestServe_ReadOnly_RejectionInvokesAuditAndMetrics(t *testing.T) {
	t.Parallel()

	auditBuf := &threadSafeBuffer{}
	audit := &Audit{w: auditBuf}

	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	handler := func(_ context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		return map[string]any{"unexpected": true}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, in, syncWriter, handler,
			WithReadOnly(true),
			WithAudit(audit),
		)
	}()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"kill_all_sessions","arguments":{}}}` + "\n"))

	_ = waitForBody(t, out, outMu, "kill_all_sessions", 3*time.Second)

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
		Tool      string `json:"tool"`
		Result    string `json:"result"`
		ErrorCode *int   `json:"error_code"`
	}
	// Trim trailing newline so the unmarshaller sees one clean object.
	if err := json.Unmarshal(bytes.TrimSpace(rec), &got); err != nil {
		t.Fatalf("decode audit: %v body=%q", err, rec)
	}
	if got.Tool != "kill_all_sessions" {
		t.Fatalf("audit.tool = %q, want kill_all_sessions", got.Tool)
	}
	if got.Result != "error" {
		t.Fatalf("audit.result = %q, want error", got.Result)
	}
	if got.ErrorCode == nil {
		t.Fatalf("audit.error_code missing; want %d", errs.CodeReadOnly)
	}
	if *got.ErrorCode != errs.CodeReadOnly {
		t.Fatalf("audit.error_code = %d, want %d", *got.ErrorCode, errs.CodeReadOnly)
	}
}

// waitForBody polls a synchronised buffer until it contains marker or
// the deadline expires. Returns the buffer contents at the time the
// marker was observed (or the final state on timeout). The polling
// interval matches the rest of the package's Serve tests so a slow
// CI runner sees the same back-off cadence.
func waitForBody(t *testing.T, out *bytes.Buffer, mu *sync.Mutex, marker string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		body := out.String()
		mu.Unlock()
		if strings.Contains(body, marker) {
			return body
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	body := out.String()
	mu.Unlock()
	t.Fatalf("did not see marker %q in body within %s; body=%q", marker, timeout, body)
	return body
}

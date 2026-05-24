package server

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRegisterTool_EmitsListChangedNotification covers the happy
// path: SetNotifier is bound, the dispatcher sees `initialize`, then
// RegisterTool fires the spec-defined notification frame exactly
// once.
func TestRegisterTool_EmitsListChangedNotification(t *testing.T) {
	t.Parallel()
	tools := &Tools{}

	var calls atomic.Int32
	tools.SetNotifier(func() { calls.Add(1) })

	// The notifier must stay silent until the server has processed
	// initialize. This mirrors the MCP spec: list-change frames are
	// only meaningful once the connection is up.
	tools.RegisterTool(map[string]any{
		"name":        "early_bird",
		"description": "registered before initialize",
		"inputSchema": map[string]any{"type": "object"},
	}, func(context.Context, json.RawMessage) (any, *rpcError) {
		return textBlock("ok"), nil
	})
	if got := calls.Load(); got != 0 {
		t.Fatalf("notifier fired %d times before initialize, want 0", got)
	}

	if _, rerr := tools.Handle(context.Background(), "initialize", nil); rerr != nil {
		t.Fatalf("initialize: %v", rerr)
	}

	// Post-initialize, every Register/Unregister call must produce
	// exactly one notification — no debouncing, no batching.
	tools.RegisterTool(map[string]any{
		"name":        "late_bird",
		"description": "registered after initialize",
		"inputSchema": map[string]any{"type": "object"},
	}, func(context.Context, json.RawMessage) (any, *rpcError) {
		return textBlock("ok"), nil
	})
	if got := calls.Load(); got != 1 {
		t.Fatalf("notifier fired %d times for one register, want 1", got)
	}

	// tools/list must reflect both the early and late registrations:
	// the early one came before initialize but the surface still
	// retains it (we only suppressed the wire-level emission, not
	// the registration itself).
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing, ok := res.(map[string]any)["tools"].([]map[string]any)
	if !ok {
		t.Fatalf("tools/list result has unexpected shape: %#v", res)
	}
	names := toolNames(listing)
	if !names["early_bird"] {
		t.Fatalf("tools/list missing early_bird: %v", names)
	}
	if !names["late_bird"] {
		t.Fatalf("tools/list missing late_bird: %v", names)
	}
}

// TestUnregisterTool_EmitsListChangedNotification verifies the
// inverse of the register case: dropping a tool flips the notifier
// once and removes the entry from tools/list.
func TestUnregisterTool_EmitsListChangedNotification(t *testing.T) {
	t.Parallel()
	tools := &Tools{}

	var calls atomic.Int32
	tools.SetNotifier(func() { calls.Add(1) })

	if _, rerr := tools.Handle(context.Background(), "initialize", nil); rerr != nil {
		t.Fatalf("initialize: %v", rerr)
	}
	tools.RegisterTool(map[string]any{
		"name":        "throwaway",
		"description": "to be unregistered",
		"inputSchema": map[string]any{"type": "object"},
	}, func(context.Context, json.RawMessage) (any, *rpcError) {
		return textBlock("ok"), nil
	})
	// Reset the counter to isolate the unregister-side behaviour.
	calls.Store(0)

	tools.UnregisterTool("throwaway")
	if got := calls.Load(); got != 1 {
		t.Fatalf("notifier fired %d times for one unregister, want 1", got)
	}

	// Unregistering a name that is not present must NOT fire the
	// notifier — there is nothing to tell the client about.
	tools.UnregisterTool("does_not_exist")
	if got := calls.Load(); got != 1 {
		t.Fatalf("notifier fired %d times after no-op unregister, want still 1", got)
	}

	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing, _ := res.(map[string]any)["tools"].([]map[string]any)
	if toolNames(listing)["throwaway"] {
		t.Fatalf("tools/list still contains unregistered tool: %v", listing)
	}
}

// TestInitialize_AdvertisesListChangedCapability pins the handshake
// contract: the initialize response must declare tools.listChanged
// so MCP clients know to subscribe to the notification surface this
// PR added.
func TestInitialize_AdvertisesListChangedCapability(t *testing.T) {
	t.Parallel()
	tools := &Tools{}

	res, rerr := tools.Handle(context.Background(), "initialize", nil)
	if rerr != nil {
		t.Fatalf("initialize: %v", rerr)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("initialize result is not a map: %#v", res)
	}
	caps, ok := m["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities not a map: %#v", m["capabilities"])
	}
	toolsCap, ok := caps["tools"].(map[string]any)
	if !ok {
		t.Fatalf("tools capability not a map: %#v", caps["tools"])
	}
	listChanged, ok := toolsCap["listChanged"].(bool)
	if !ok {
		t.Fatalf("tools.listChanged not a bool: %#v", toolsCap["listChanged"])
	}
	if !listChanged {
		t.Fatalf("tools.listChanged = false, want true")
	}
}

// TestServe_WiresListChangedNotificationToStdout pins the
// integration contract end-to-end: WithToolsListChangedNotifier
// hands a writeMu-bound emitter to *Tools, and a RegisterTool call
// post-initialize causes the dispatcher to emit a spec-shaped
// notification frame on stdout (no id, no params). Without the wire
// hook the notification never reaches the client, so this test
// guards against future refactors that drop the SetNotifier call in
// main.go or the WithToolsListChangedNotifier option in Serve.
func TestServe_WiresListChangedNotificationToStdout(t *testing.T) {
	t.Parallel()
	tools := &Tools{}

	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(
			ctx, in, syncWriter, tools.Handle,
			WithToolsListChangedNotifier(tools.SetNotifier),
		)
	}()

	// Drive the handshake so initialized is true before we register.
	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n"))
	waitForOutput(t, out, outMu, `"protocolVersion"`, 3*time.Second)

	// Register a tool and assert the notification frame appears on
	// stdout. The frame must be a JSON-RPC notification (no id,
	// method = notifications/tools/list_changed).
	tools.RegisterTool(map[string]any{
		"name":        "wire_check",
		"description": "for the wire-level test",
		"inputSchema": map[string]any{"type": "object"},
	}, func(context.Context, json.RawMessage) (any, *rpcError) {
		return textBlock("ok"), nil
	})
	waitForOutput(t, out, outMu, "notifications/tools/list_changed", 3*time.Second)

	in.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after EOF")
	}

	outMu.Lock()
	body := out.String()
	outMu.Unlock()
	// Every line of stdout must be a complete JSON object — the
	// notification cannot interleave with the initialize response or
	// share a line with it. We split on '\n', skip empties, and
	// verify the notification line decodes to the spec shape.
	var sawNotification bool
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatalf("non-JSON line on stdout %q: %v", raw, err)
		}
		if frame["method"] != "notifications/tools/list_changed" {
			continue
		}
		sawNotification = true
		if frame["jsonrpc"] != "2.0" {
			t.Fatalf("notification missing jsonrpc=2.0: %v", frame)
		}
		if _, present := frame["id"]; present {
			t.Fatalf("notification carried an id (must be absent per spec): %v", frame)
		}
		if _, present := frame["params"]; present {
			t.Fatalf("notification carried params (must be absent per spec): %v", frame)
		}
	}
	if !sawNotification {
		t.Fatalf("expected list_changed notification on stdout, got %q", body)
	}
}

// toolNames flattens a tools/list listing into a name set so tests
// can spot-check membership without iterating manually each time.
func toolNames(listing []map[string]any) map[string]bool {
	out := make(map[string]bool, len(listing))
	for _, def := range listing {
		if name, ok := def["name"].(string); ok {
			out[name] = true
		}
	}
	return out
}

// waitForOutput polls the (mutex-guarded) stdout buffer for needle
// up to timeout. It is the standard pattern the existing dispatcher
// tests use; pulling it into a helper keeps the new test cases
// readable without copying the loop body twice.
func waitForOutput(t *testing.T, out *bytes.Buffer, mu *sync.Mutex, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		body := out.String()
		mu.Unlock()
		if strings.Contains(body, needle) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	body := out.String()
	mu.Unlock()
	t.Fatalf("timed out waiting for %q in stdout, got %q", needle, body)
}

package server

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestHandle_StartServer drives the JSON-RPC round-trip for the
// start_server tool: dispatch a tools/call, decode the canonical
// `{"started": true}` ack, and assert the controller's tmux daemon
// actually came up by stat-ing the socket file. This is the load-bearing
// happy path — agents pre-warming the daemon hit exactly this code on
// every startup hook.
func TestHandle_StartServer(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	params := mustJSON(t, map[string]any{
		"name":      "start_server",
		"arguments": map[string]any{},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("start_server: %s", rerr.Message)
	}

	body := extractText(t, res)
	var payload struct {
		Started bool `json:"started"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode start_server response: %v\nbody=%s", err, body)
	}
	if !payload.Started {
		t.Fatalf("expected started=true, got payload=%+v (body=%s)", payload, body)
	}

	// The visible side effect: the controller's socket file exists and
	// is a unix-domain socket. tmux only creates the socket once the
	// daemon is bound, so the existence of the socket node is the
	// cheapest "did the daemon really come up?" probe we have.
	info, err := os.Stat(tools.Ctl.Socket())
	if err != nil {
		t.Fatalf("stat socket %q after start_server: %v", tools.Ctl.Socket(), err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket %q exists but mode is %s, want a unix-domain socket",
			tools.Ctl.Socket(), info.Mode())
	}
}

// TestHandle_StartServer_Idempotent pins the no-op-on-second-call
// contract end-to-end through the dispatcher. tmux's `start-server` is
// itself idempotent, but the wrapper has its own error-mapping path
// (Controller.run) and a regression there would surface as a spurious
// failure on the second call — exactly the path agents whose startup
// hook fires twice (e.g. retried supervisor restarts) hit.
func TestHandle_StartServer_Idempotent(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	params := mustJSON(t, map[string]any{
		"name":      "start_server",
		"arguments": map[string]any{},
	})
	for i := 0; i < 2; i++ {
		res, rerr := tools.Handle(ctx, "tools/call", params)
		if rerr != nil {
			t.Fatalf("start_server call %d: %s", i+1, rerr.Message)
		}
		body := extractText(t, res)
		var payload struct {
			Started bool `json:"started"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("decode start_server response (call %d): %v\nbody=%s", i+1, err, body)
		}
		if !payload.Started {
			t.Fatalf("call %d: expected started=true, got %+v", i+1, payload)
		}
	}
}

// TestHandle_StartServer_NoArgsAccepted exercises the "callable without
// an arguments object at all" contract: tmux's `start-server` takes no
// flags, the schema declares no fields, and clients should be able to
// call it with a literal `null` arguments value the way some MCP
// clients serialise an empty argument set. A regression that demanded
// `{}` here would silently break every minimal client.
func TestHandle_StartServer_NoArgsAccepted(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	params := mustJSON(t, map[string]any{
		"name":      "start_server",
		"arguments": nil,
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("start_server with null arguments: %s", rerr.Message)
	}
	body := extractText(t, res)
	if body == "" {
		t.Fatal("expected non-empty body from start_server")
	}
}

// TestHandle_StartServer_ListedInTools confirms the init()-time
// registration actually wired start_server into tools/list. Without
// this guard a regression in the package-init append could silently
// drop the tool from the surface even though the dispatcher still
// recognised it.
func TestHandle_StartServer_ListedInTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] == "start_server" {
			return
		}
	}
	t.Fatal("tools/list missing start_server")
}

// TestStartServer_NotInReadOnlyAllowlist pins the policy: start_server
// spawns a daemon process and is therefore mutating, so a -read-only
// operator must not be able to invoke it. Adding the tool to the
// allowlist would silently let a read-only agent bring up a daemon
// they only meant to inspect.
func TestStartServer_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("start_server") {
		t.Fatal("start_server must NOT be in the read-only allowlist (it spawns a daemon process)")
	}
}

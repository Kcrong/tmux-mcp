package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// loadBufferTestSetup spins up a fresh *Tools with an anchor session
// (so the tmux server is definitely running — buffers live on the
// server, and list/show against a server-less socket reports
// "error connecting" rather than "no buffers"), and returns a
// pre-bound `call` helper plus the deadline context. Mirrors
// setBufferTestSetup so the two suites stay structurally aligned —
// the only difference is the anchor session name, kept distinct so
// parallel test runs don't collide on the per-controller registry.
//
// Each caller must invoke t.Parallel() itself — t.Helper() inside a
// helper does not propagate t.Parallel, and the user-facing
// concurrency contract is "one tmux server per top-level test".
func loadBufferTestSetup(t *testing.T) (
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

	// Anchor with a real session so the tmux server is definitely up;
	// buffers live on the server and require it to be running for any
	// list-buffers / show-buffer probe in the assertions below.
	call("session_create", map[string]any{
		"name": "anchor_load_buf", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "anchor_load_buf"},
			}))
	})
	return tools, call, ctx
}

// decodeLoadBuffer pulls the {"loaded": ..., "name": ...} envelope out
// of the tools/call result so the assertions in each test stay focused
// on the field that matters.
func decodeLoadBuffer(t *testing.T, result any) (loaded bool, name string) {
	t.Helper()
	body := extractText(t, result)
	var obj struct {
		Loaded bool   `json:"loaded"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode load_buffer body %q: %v", body, err)
	}
	return obj.Loaded, obj.Name
}

// TestHandle_LoadBuffer_AutoNameLands drives case (a) of the surface
// contract: writing a buffer without `name` resolves to one of tmux's
// auto-assigned `bufferN` names, and the resolved name actually
// appears in `tmux list-buffers`. Mirrors the equivalent set_buffer
// test so the auto-naming behaviour is locked across both writers.
func TestHandle_LoadBuffer_AutoNameLands(t *testing.T) {
	t.Parallel()
	tools, call, _ := loadBufferTestSetup(t)

	const payload = "hello-from-load-auto-name"
	loaded, name := decodeLoadBuffer(t, call("load_buffer", map[string]any{
		"data": payload,
	}))
	if !loaded {
		t.Fatalf("load_buffer loaded=false, want true")
	}
	if !strings.HasPrefix(name, "buffer") {
		t.Fatalf("auto name %q must start with `buffer`; tmux assigns bufferN when -b is omitted", name)
	}

	// Probe tmux directly: list-buffers should know about the name we
	// just received and show-buffer should round-trip the payload.
	socket := tools.Ctl.Socket()
	listing := tmuxRunSetBuffer(t, socket, "list-buffers", "-F", "#{buffer_name}")
	if !strings.Contains(listing, name) {
		t.Fatalf("list-buffers does not contain %q; got\n%s", name, listing)
	}
	body := tmuxRunSetBuffer(t, socket, "show-buffer", "-b", name)
	if body != payload {
		t.Errorf("show-buffer(%q) = %q, want %q", name, body, payload)
	}
}

// TestHandle_LoadBuffer_NamedRoundTrips drives case (b): writing with
// an explicit `name` echoes the same name back verbatim, and the
// payload round-trips through `tmux show-buffer -b NAME`. Locks the
// load-bearing path for an agent stashing a snippet under a known
// name so a follow-up tool call can pick it up.
func TestHandle_LoadBuffer_NamedRoundTrips(t *testing.T) {
	t.Parallel()
	tools, call, _ := loadBufferTestSetup(t)

	const want = "the quick brown fox jumps over the lazy dog"
	loaded, name := decodeLoadBuffer(t, call("load_buffer", map[string]any{
		"data": want,
		"name": "lb_pinned_named",
	}))
	if !loaded {
		t.Fatalf("load_buffer loaded=false, want true")
	}
	if name != "lb_pinned_named" {
		t.Errorf("load_buffer echoed name = %q, want %q", name, "lb_pinned_named")
	}

	body := tmuxRunSetBuffer(t, tools.Ctl.Socket(), "show-buffer", "-b", "lb_pinned_named")
	if body != want {
		t.Errorf("show-buffer(lb_pinned_named) = %q, want %q", body, want)
	}
}

// TestHandle_LoadBuffer_AppendConcatenates drives case (c): a second
// load_buffer call with `append: true` and the same `name` extends
// the existing buffer rather than replacing it. Mirrors set_buffer's
// own append test so the behaviour stays aligned across writers.
func TestHandle_LoadBuffer_AppendConcatenates(t *testing.T) {
	t.Parallel()
	tools, call, _ := loadBufferTestSetup(t)

	const head = "head|"
	const tail = "tail"
	call("load_buffer", map[string]any{
		"data": head,
		"name": "lb_appended",
	})
	loaded, name := decodeLoadBuffer(t, call("load_buffer", map[string]any{
		"data":   tail,
		"name":   "lb_appended",
		"append": true,
	}))
	if !loaded {
		t.Fatalf("load_buffer loaded=false, want true")
	}
	if name != "lb_appended" {
		t.Errorf("load_buffer echoed name = %q, want %q", name, "lb_appended")
	}

	body := tmuxRunSetBuffer(t, tools.Ctl.Socket(), "show-buffer", "-b", "lb_appended")
	if body != head+tail {
		t.Errorf("show-buffer(lb_appended) = %q, want %q", body, head+tail)
	}
}

// TestHandle_LoadBuffer_LargePayload exercises the load-over-stdin
// rationale at the MCP boundary. 5 KiB is comfortably below the 1 MiB
// frame cap but already large enough that pushing it through
// `set-buffer DATA` would feel awkward on platforms with a tight
// ARG_MAX. The payload includes a salting of newlines / shell metachars
// so any accidental shell-eval'ing would surface immediately.
func TestHandle_LoadBuffer_LargePayload(t *testing.T) {
	t.Parallel()
	tools, call, _ := loadBufferTestSetup(t)

	raw := make([]byte, 5120/4*3)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(raw) +
		"\n;rm -rf /\nline2\t$(echo nope)"
	if len(encoded) < 5*1024 {
		t.Fatalf("test payload too small: %d bytes", len(encoded))
	}

	loaded, name := decodeLoadBuffer(t, call("load_buffer", map[string]any{
		"data": encoded,
		"name": "lb_big_payload",
	}))
	if !loaded {
		t.Fatalf("load_buffer loaded=false, want true")
	}
	if name != "lb_big_payload" {
		t.Errorf("name = %q, want %q", name, "lb_big_payload")
	}
	body := tmuxRunSetBuffer(t, tools.Ctl.Socket(), "show-buffer", "-b", "lb_big_payload")
	if body != encoded {
		t.Errorf("show-buffer length = %d, want %d (round-trip mismatch)", len(body), len(encoded))
	}
}

// TestHandle_LoadBuffer_EmptyDataAccepted locks the "empty data is
// not rejected at the boundary" contract: passing `data: ""` must
// succeed. tmux's behaviour for empty payloads varies by version (3.4
// drops the buffer entirely), so this assertion focuses on the MCP
// boundary contract — no -32602 from the validator and the underlying
// load-buffer command returns success.
func TestHandle_LoadBuffer_EmptyDataAccepted(t *testing.T) {
	t.Parallel()
	_, call, _ := loadBufferTestSetup(t)

	loaded, _ := decodeLoadBuffer(t, call("load_buffer", map[string]any{
		"data": "",
		"name": "lb_empty_one",
	}))
	if !loaded {
		t.Fatalf("load_buffer loaded=false, want true")
	}
}

// TestHandle_LoadBuffer_RejectsMissingData pins the up-front guard:
// without `data` the call must fail with CodeInvalidParams (-32602)
// before any tmux command runs.
func TestHandle_LoadBuffer_RejectsMissingData(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "load_buffer",
		"arguments": map[string]any{},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing data")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_LoadBuffer_RejectsOversizedData enforces the 1 MiB cap.
// A 1 MiB+1 byte payload must fail with CodeInvalidParams before any
// tmux process is spawned — the load-over-stdin path can sustain much
// larger payloads at the OS level, but the MCP layer wants a
// predictable wire-frame ceiling.
func TestHandle_LoadBuffer_RejectsOversizedData(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	oversized := strings.Repeat("a", maxLoadBufferDataBytes+1)
	params := mustJSON(t, map[string]any{
		"name": "load_buffer",
		"arguments": map[string]any{
			"data": oversized,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized data")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "max 1048576 bytes") {
		t.Errorf("error msg = %q, expected to mention `max 1048576 bytes`", rerr.Message)
	}
}

// TestHandle_LoadBuffer_RejectsBadName locks the regex check on
// `name` so a stray quote or whitespace cannot slip through to the
// tmux argv. The check runs before any tmux command, so the error
// must carry CodeInvalidParams (-32602).
func TestHandle_LoadBuffer_RejectsBadName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "load_buffer",
		"arguments": map[string]any{
			"data": "anything",
			"name": "bad name with spaces",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ToolsList_IncludesLoadBuffer makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint.
func TestHandle_ToolsList_IncludesLoadBuffer(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "load_buffer" {
			return
		}
	}
	t.Fatalf("tools/list missing load_buffer")
}

// TestHandle_LoadBuffer_NotReadOnly pins that load_buffer is excluded
// from the read-only allowlist. The tool mutates server-side buffer
// state (writes a new entry, replaces an existing one, or appends),
// so an operator who armed -read-only must see it filtered out.
func TestHandle_LoadBuffer_NotReadOnly(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("load_buffer") {
		t.Fatalf("IsReadOnlyTool(\"load_buffer\") = true, want false (load_buffer mutates state)")
	}
}

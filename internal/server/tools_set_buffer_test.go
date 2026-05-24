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

// tmuxRunSetBuffer shells out to the tmux binary on PATH against the
// supplied socket and returns stdout. Used by the set_buffer suite to
// inspect tmux's view of the buffers without depending on the
// list_buffers / show_buffer MCP tools, which arrive in a separate PR
// (feat/buffer-tools, #98) that has not yet merged. Failure aborts
// the test with a stderr-bearing message so a flaky tmux build does
// not turn into a head-scratching assertion miss.
func tmuxRunSetBuffer(t *testing.T, socket string, args ...string) string {
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
		t.Fatalf("tmux %v: %v (stderr=%q)", args, runErr, stderr.String())
	}
	return stdout.String()
}

// setBufferTestSetup spins up a fresh *Tools with an anchor session
// (so the tmux server is definitely running — buffers live on the
// server, and list/show against a server-less socket reports
// "error connecting" rather than "no buffers"), and returns a
// pre-bound `call` helper plus the deadline context. Pulling the
// boilerplate into a helper keeps every test case focused on the
// assertion that actually matters.
//
// Each caller must invoke t.Parallel() itself — t.Helper() inside a
// helper does not propagate t.Parallel, and the user-facing
// concurrency contract is "one tmux server per top-level test".
func setBufferTestSetup(t *testing.T) (
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
		"name": "anchor_set_buf", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "anchor_set_buf"},
			}))
	})
	return tools, call, ctx
}

// decodeSetBuffer pulls the {"set": ..., "name": ...} envelope out of
// the tools/call result so the assertions in each test stay focused
// on the field that matters.
func decodeSetBuffer(t *testing.T, result any) (set bool, name string) {
	t.Helper()
	body := extractText(t, result)
	var obj struct {
		Set  bool   `json:"set"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode set_buffer body %q: %v", body, err)
	}
	return obj.Set, obj.Name
}

// TestHandle_SetBuffer_AutoNameLands drives case (a) of the surface
// contract: writing a buffer without `name` resolves to one of tmux's
// auto-assigned `bufferN` names, and the resolved name actually
// appears in `tmux list-buffers`. This stand-in proves the MCP
// boundary wired the right tmux command — list_buffers (the MCP read
// tool) does not yet exist on main, so we probe tmux directly to keep
// this PR independent of feat/buffer-tools.
func TestHandle_SetBuffer_AutoNameLands(t *testing.T) {
	t.Parallel()
	tools, call, _ := setBufferTestSetup(t)

	const payload = "hello-from-auto-name"
	set, name := decodeSetBuffer(t, call("set_buffer", map[string]any{
		"data": payload,
	}))
	if !set {
		t.Fatalf("set_buffer set=false, want true")
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
	// tmux does not append a trailing newline to show-buffer.
	if body != payload {
		t.Errorf("show-buffer(%q) = %q, want %q", name, body, payload)
	}
}

// TestHandle_SetBuffer_NamedRoundTrips drives case (b): writing with
// an explicit `name` echoes the same name back verbatim, and the
// payload round-trips through `tmux show-buffer -b NAME`. This is the
// load-bearing path for an agent stashing a snippet under a known
// name so a follow-up tool call can pick it up — show_buffer arrives
// in feat/buffer-tools (#98) so we lean on the tmux CLI directly to
// keep the assertion independent of that PR's merge order.
func TestHandle_SetBuffer_NamedRoundTrips(t *testing.T) {
	t.Parallel()
	tools, call, _ := setBufferTestSetup(t)

	const want = "the quick brown fox jumps"
	set, name := decodeSetBuffer(t, call("set_buffer", map[string]any{
		"data": want,
		"name": "pinned_named",
	}))
	if !set {
		t.Fatalf("set_buffer set=false, want true")
	}
	if name != "pinned_named" {
		t.Errorf("set_buffer echoed name = %q, want %q", name, "pinned_named")
	}

	body := tmuxRunSetBuffer(t, tools.Ctl.Socket(), "show-buffer", "-b", "pinned_named")
	if body != want {
		t.Errorf("show-buffer(pinned_named) = %q, want %q", body, want)
	}
}

// TestHandle_SetBuffer_AppendConcatenates drives case (c): a second
// set_buffer call with `append: true` and the same `name` should
// extend the existing buffer rather than replacing it. Mirrors
// `tmux set-buffer -a -b NAME ...` end-to-end through the dispatcher.
func TestHandle_SetBuffer_AppendConcatenates(t *testing.T) {
	t.Parallel()
	tools, call, _ := setBufferTestSetup(t)

	// First write replaces (or creates) the buffer.
	const head = "head|"
	const tail = "tail"
	call("set_buffer", map[string]any{
		"data": head,
		"name": "appended",
	})
	// Second write appends.
	set, name := decodeSetBuffer(t, call("set_buffer", map[string]any{
		"data":   tail,
		"name":   "appended",
		"append": true,
	}))
	if !set {
		t.Fatalf("set_buffer set=false, want true")
	}
	if name != "appended" {
		t.Errorf("set_buffer echoed name = %q, want %q", name, "appended")
	}

	body := tmuxRunSetBuffer(t, tools.Ctl.Socket(), "show-buffer", "-b", "appended")
	if body != head+tail {
		t.Errorf("show-buffer(appended) = %q, want %q", body, head+tail)
	}
}

// TestHandle_SetBuffer_AppendCreatesWhenMissing pins the documented
// "append against a missing named buffer creates it" branch. tmux
// itself silently creates the buffer in this case (no error), so the
// MCP wrapper inherits that behaviour — agents using `append=true`
// don't need to first probe whether the buffer exists.
func TestHandle_SetBuffer_AppendCreatesWhenMissing(t *testing.T) {
	t.Parallel()
	tools, call, _ := setBufferTestSetup(t)

	const payload = "first-write-via-append"
	set, name := decodeSetBuffer(t, call("set_buffer", map[string]any{
		"data":   payload,
		"name":   "born_via_append",
		"append": true,
	}))
	if !set {
		t.Fatalf("set_buffer set=false, want true")
	}
	if name != "born_via_append" {
		t.Errorf("name = %q, want %q", name, "born_via_append")
	}
	body := tmuxRunSetBuffer(t, tools.Ctl.Socket(), "show-buffer", "-b", "born_via_append")
	if body != payload {
		t.Errorf("show-buffer(born_via_append) = %q, want %q", body, payload)
	}
}

// TestHandle_SetBuffer_EmptyDataAccepted locks the "empty data is not
// rejected at the boundary" contract: passing `data: ""` must succeed
// (no -32602 from the validator). tmux itself treats an empty payload
// as a no-op — it accepts the command but nothing lands in the buffer
// table on tmux 3.4, which is what an agent that wanted to "blank"
// the buffer would expect anyway. The MCP layer's job is to not
// invent an extra rejection on top of what tmux already does; that
// is what this test pins.
func TestHandle_SetBuffer_EmptyDataAccepted(t *testing.T) {
	t.Parallel()
	_, call, _ := setBufferTestSetup(t)

	// No assertion on round-tripping the buffer payload — tmux's
	// own behaviour for empty payloads varies across versions (3.4
	// drops the buffer; later versions may keep an empty one), and
	// the boundary-layer guarantee we actually care about is just
	// "validator does not reject" + "downstream tmux call returns
	// success".
	set, _ := decodeSetBuffer(t, call("set_buffer", map[string]any{
		"data": "",
		"name": "empty_one",
	}))
	if !set {
		t.Fatalf("set_buffer set=false, want true")
	}
}

// TestHandle_SetBuffer_RejectsMissingData pins the up-front guard:
// without `data` the call must fail with CodeInvalidParams (-32602)
// before any tmux command runs.
func TestHandle_SetBuffer_RejectsMissingData(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "set_buffer",
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

// TestHandle_SetBuffer_RejectsOversizedData enforces the 1 MiB cap.
// A 1 MiB+1 byte payload must fail with CodeInvalidParams before any
// tmux process is spawned — otherwise tmux happily allocates the
// buffer and the JSON-RPC writer is left holding the bag.
func TestHandle_SetBuffer_RejectsOversizedData(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	oversized := strings.Repeat("a", maxSetBufferDataBytes+1)
	params := mustJSON(t, map[string]any{
		"name": "set_buffer",
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

// TestHandle_SetBuffer_RejectsBadName locks the regex check on `name`
// so a stray quote or whitespace cannot slip through to the tmux
// argv. The check runs before any tmux command, so the error must
// carry CodeInvalidParams (-32602).
func TestHandle_SetBuffer_RejectsBadName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "set_buffer",
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

// TestHandle_ToolsList_IncludesSetBuffer makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint.
func TestHandle_ToolsList_IncludesSetBuffer(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "set_buffer" {
			return
		}
	}
	t.Fatalf("tools/list missing set_buffer")
}

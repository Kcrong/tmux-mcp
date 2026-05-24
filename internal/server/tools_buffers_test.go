package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// tmuxRun shells out to the tmux binary on PATH against socket and
// returns stdout. Used by buffer tests to seed paste buffers without
// extending the controller's public surface — the controller exposes
// only the operations the production server needs, and "set-buffer"
// is a test-only fixture concern. Failure aborts the test with a
// clear stderr-bearing message so a flaky tmux build does not turn
// into a head-scratching assertion miss.
func tmuxRun(t *testing.T, socket string, args ...string) string {
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

// bufferTestSetup spins up a fresh *Tools with an anchor session so
// the tmux server is definitely running, seeds two buffers via
// `tmux set-buffer`, and returns a `call` helper plus the deadline
// context. Pulling the boilerplate into a helper keeps every test
// case focused on the assertion that actually matters.
//
// Each caller must invoke t.Parallel() itself — t.Helper() inside a
// helper does not propagate t.Parallel, and the user-facing
// concurrency contract is "one tmux server per top-level test".
func bufferTestSetup(t *testing.T) (
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
	// buffers live on the server and require it to be running.
	call("session_create", map[string]any{
		"name": "anchor", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "anchor"}}))
	})
	return tools, call, ctx
}

// seedBuffer drives `tmux set-buffer` directly on the controller's
// socket so we don't conflate "the buffer tools work" with "the test
// fixtures work". When name is empty the auto-naming path runs (tmux
// assigns "bufferN"); when non-empty `-b NAME` pins the buffer. The
// controller deliberately does not expose set-buffer (production
// tooling has no need), so this is a test-only fixture.
func seedBuffer(t *testing.T, tools *Tools, name, data string) {
	t.Helper()
	socket := tools.Ctl.Socket()
	if socket == "" {
		t.Fatalf("controller socket is empty")
	}
	args := []string{"set-buffer"}
	if name != "" {
		args = append(args, "-b", name)
	}
	args = append(args, data)
	tmuxRun(t, socket, args...)
}

// TestHandle_ListBuffers_EmptyOnFreshServer pins the empty-list
// contract: no buffers stored yet → `{"buffers": []}` with no error.
// MCP clients rely on the empty array (rather than null) so iterating
// the response without a nil guard is safe.
func TestHandle_ListBuffers_EmptyOnFreshServer(t *testing.T) {
	t.Parallel()
	_, call, _ := bufferTestSetup(t)

	listText := extractText(t, call("list_buffers", map[string]any{}))
	var listObj struct {
		Buffers []map[string]any `json:"buffers"`
	}
	if err := json.Unmarshal([]byte(listText), &listObj); err != nil {
		t.Fatalf("decode list_buffers: %v\nbody=%s", err, listText)
	}
	if listObj.Buffers == nil {
		t.Fatalf("buffers field decoded to nil; want empty array (body=%s)", listText)
	}
	if len(listObj.Buffers) != 0 {
		t.Fatalf("expected zero buffers on fresh server, got %d (body=%s)", len(listObj.Buffers), listText)
	}
}

// TestHandle_ListBuffers_HappyPath drives the populated case end to
// end through the dispatcher: seed two buffers, list them, and assert
// every documented field (name / size / created_at) is present and
// well-formed.
func TestHandle_ListBuffers_HappyPath(t *testing.T) {
	t.Parallel()
	tools, call, _ := bufferTestSetup(t)

	seedBuffer(t, tools, "", "hello")
	beforePinned := time.Now().UTC().Add(-time.Second)
	seedBuffer(t, tools, "pinned", "world")
	afterPinned := time.Now().UTC().Add(time.Second)

	listText := extractText(t, call("list_buffers", map[string]any{}))
	var listObj struct {
		Buffers []struct {
			Name      string `json:"name"`
			Size      int    `json:"size"`
			CreatedAt string `json:"created_at"`
		} `json:"buffers"`
	}
	if err := json.Unmarshal([]byte(listText), &listObj); err != nil {
		t.Fatalf("decode list_buffers: %v\nbody=%s", err, listText)
	}
	if len(listObj.Buffers) != 2 {
		t.Fatalf("expected 2 buffers, got %d (body=%s)", len(listObj.Buffers), listText)
	}

	byName := make(map[string]struct {
		Name      string `json:"name"`
		Size      int    `json:"size"`
		CreatedAt string `json:"created_at"`
	}, len(listObj.Buffers))
	for _, b := range listObj.Buffers {
		byName[b.Name] = b
	}
	auto, ok := byName["buffer0"]
	if !ok {
		t.Fatalf("expected auto-named buffer0, got %v", listObj.Buffers)
	}
	if auto.Size != len("hello") {
		t.Errorf("buffer0 size = %d, want %d", auto.Size, len("hello"))
	}
	pinned, ok := byName["pinned"]
	if !ok {
		t.Fatalf("expected pinned buffer, got %v", listObj.Buffers)
	}
	if pinned.Size != len("world") {
		t.Errorf("pinned size = %d, want %d", pinned.Size, len("world"))
	}
	// created_at must parse as RFC3339 and fall within the window we
	// captured around the seed call (loose, to absorb the controller's
	// own latency).
	created, err := time.Parse(time.RFC3339, pinned.CreatedAt)
	if err != nil {
		t.Fatalf("pinned created_at %q does not parse as RFC3339: %v", pinned.CreatedAt, err)
	}
	if created.Before(beforePinned) || created.After(afterPinned) {
		t.Errorf("pinned created_at %s outside [%s..%s]", created, beforePinned, afterPinned)
	}
}

// TestHandle_ShowBuffer_DefaultDumpsMostRecent confirms the empty-name
// path resolves to the most-recently-added buffer. Agents that just
// called set-buffer can rely on this — they don't have to round-trip
// through list_buffers to learn the assigned name.
func TestHandle_ShowBuffer_DefaultDumpsMostRecent(t *testing.T) {
	t.Parallel()
	tools, call, _ := bufferTestSetup(t)

	seedBuffer(t, tools, "", "first")
	seedBuffer(t, tools, "", "second")

	// Two equivalent forms: explicit empty-string name, and no name
	// field at all. Both must resolve to the same buffer so the schema
	// stays back-compat with clients that omit the field entirely.
	for _, args := range []map[string]any{
		{},
		{"name": ""},
	} {
		showText := extractText(t, call("show_buffer", args))
		var showObj struct {
			Name string `json:"name"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal([]byte(showText), &showObj); err != nil {
			t.Fatalf("decode show_buffer: %v\nbody=%s", err, showText)
		}
		if showObj.Data != "second" {
			t.Errorf("show_buffer default data = %q, want %q (args=%v)", showObj.Data, "second", args)
		}
		if showObj.Name != "" {
			t.Errorf("show_buffer default name echoed = %q, want empty (args=%v)", showObj.Name, args)
		}
	}
}

// TestHandle_ShowBuffer_NamedReturnsSpecific exercises the -b path
// through the dispatcher: seed two buffers, dump one by name, and
// confirm the contents round-trip exactly.
func TestHandle_ShowBuffer_NamedReturnsSpecific(t *testing.T) {
	t.Parallel()
	tools, call, _ := bufferTestSetup(t)

	const want = "the quick brown fox"
	seedBuffer(t, tools, "named", want)
	// Add a decoy so the most-recent default would pick a different
	// buffer — proves -b is honoured.
	seedBuffer(t, tools, "", "decoy")

	showText := extractText(t, call("show_buffer", map[string]any{"name": "named"}))
	var showObj struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(showText), &showObj); err != nil {
		t.Fatalf("decode show_buffer: %v\nbody=%s", err, showText)
	}
	if showObj.Data != want {
		t.Errorf("show_buffer(named) data = %q, want %q", showObj.Data, want)
	}
	if showObj.Name != "named" {
		t.Errorf("show_buffer(named) echoed name = %q, want %q", showObj.Name, "named")
	}
}

// TestHandle_ShowBuffer_MissingMapsCode pins the wire contract for an
// unknown buffer name: the JSON-RPC error code must surface as
// CodeSessionNotFound (-32000) so MCP clients can branch on a stable
// code rather than the (version-specific) tmux stderr text.
func TestHandle_ShowBuffer_MissingMapsCode(t *testing.T) {
	t.Parallel()
	tools, _, ctx := bufferTestSetup(t)

	params := mustJSON(t, map[string]any{
		"name":      "show_buffer",
		"arguments": map[string]any{"name": "ghost_buffer_nonexistent"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for missing buffer, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_ShowBuffer_RejectsBadName locks the regex check on
// `name` so a stray quote/whitespace can't slip through to the tmux
// argv. The check runs before any tmux command, so the error must
// carry CodeInvalidParams (-32602).
func TestHandle_ShowBuffer_RejectsBadName(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "show_buffer",
		"arguments": map[string]any{"name": "bad name with spaces"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad buffer name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ToolsList_IncludesBufferTools makes sure tools/list
// advertises both new tools so MCP clients can discover them via the
// schema endpoint.
func TestHandle_ToolsList_IncludesBufferTools(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	want := map[string]bool{"list_buffers": false, "show_buffer": false}
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "" {
			if _, ok := want[name]; ok {
				want[name] = true
			}
		}
	}
	for n, ok := range want {
		if !ok {
			t.Errorf("tools/list missing %q", n)
		}
	}
}

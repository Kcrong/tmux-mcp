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

// tmuxRunBindKey shells out to the tmux binary on PATH against the
// supplied socket and returns stdout. Used by the bind_key suite to
// inspect tmux's view of the bindings independent of the list_keys
// MCP tool — keeps the assertion focused on what bind_key wrote
// rather than coupling two MCP surfaces in one test.
func tmuxRunBindKey(t *testing.T, socket string, args ...string) string {
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

// bindKeyTestSetup spins up a fresh *Tools with an anchor session (so
// the tmux server is definitely running — bind-key writes server-wide
// state and needs the daemon up) and returns a pre-bound `call` helper
// plus the deadline context. Each caller must invoke t.Parallel() —
// t.Helper() inside a helper does not propagate t.Parallel.
func bindKeyTestSetup(t *testing.T) (
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
	call("session_create", map[string]any{
		"name": "anchor_bind_key", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "anchor_bind_key"},
			}))
	})
	return tools, call, ctx
}

// decodeBindKey pulls the {"bound", "key", "table"} envelope out of
// the tools/call result so each test can focus on the field that
// matters rather than re-deriving the JSON shape.
func decodeBindKey(t *testing.T, result any) (bound bool, key, table string) {
	t.Helper()
	body := extractText(t, result)
	var obj struct {
		Bound bool   `json:"bound"`
		Key   string `json:"key"`
		Table string `json:"table"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode bind_key body %q: %v", body, err)
	}
	return obj.Bound, obj.Key, obj.Table
}

// TestHandle_BindKey_HappyPath_PrefixTable is the load-bearing default
// path: a write to the prefix table (empty `key_table`) lands the
// binding and `tmux list-keys -T prefix` echoes the same {key, command}
// back through the dispatcher. The response envelope must report
// bound=true and round-trip the caller's key/table so an agent can
// confirm what tmux now thinks.
func TestHandle_BindKey_HappyPath_PrefixTable(t *testing.T) {
	t.Parallel()
	tools, call, _ := bindKeyTestSetup(t)

	const key = "F12"
	const cmd = "display-message hello-via-bind-key"
	bound, gotKey, gotTable := decodeBindKey(t, call("bind_key", map[string]any{
		"key":     key,
		"command": cmd,
	}))
	if !bound {
		t.Fatal("bind_key bound=false, want true")
	}
	if gotKey != key {
		t.Errorf("response key = %q, want %q", gotKey, key)
	}
	if gotTable != "" {
		t.Errorf("response table = %q, want empty (default-table branch)", gotTable)
	}

	// Probe tmux directly: list-keys -T prefix should now contain the
	// chord we just bound, with the command preserved verbatim.
	listing := tmuxRunBindKey(t, tools.Ctl.Socket(), "list-keys", "-T", "prefix")
	if !strings.Contains(listing, key) {
		t.Fatalf("list-keys -T prefix does not contain %q; got\n%s", key, listing)
	}
	if !strings.Contains(listing, "display-message") || !strings.Contains(listing, "hello-via-bind-key") {
		t.Fatalf("list-keys -T prefix missing command fragments; got\n%s", listing)
	}
}

// TestHandle_BindKey_CustomTable pins the `key_table` plumbing: when
// the caller pins copy-mode, the binding lands there (and the response
// echoes the same table) — and the same chord must NOT leak into
// prefix. Mirrors the controller-level test but goes end-to-end through
// the MCP dispatcher to catch any plumbing drift in the server layer.
func TestHandle_BindKey_CustomTable(t *testing.T) {
	t.Parallel()
	tools, call, _ := bindKeyTestSetup(t)

	const key = "F11"
	const cmd = "display-message custom-table-via-mcp"
	bound, _, gotTable := decodeBindKey(t, call("bind_key", map[string]any{
		"key":       key,
		"command":   cmd,
		"key_table": "copy-mode",
	}))
	if !bound {
		t.Fatal("bound=false, want true")
	}
	if gotTable != "copy-mode" {
		t.Errorf("response table = %q, want \"copy-mode\"", gotTable)
	}

	socket := tools.Ctl.Socket()
	listingCopy := tmuxRunBindKey(t, socket, "list-keys", "-T", "copy-mode")
	if !strings.Contains(listingCopy, key) {
		t.Fatalf("copy-mode listing missing %q; got\n%s", key, listingCopy)
	}
	listingPrefix := tmuxRunBindKey(t, socket, "list-keys", "-T", "prefix")
	if strings.Contains(listingPrefix, " "+key+" ") {
		t.Fatalf("F11 leaked into prefix table; got\n%s", listingPrefix)
	}
}

// TestHandle_BindKey_RepeatableFlag exercises the `repeatable=true`
// path. We can't easily detect the `-r` decoration through list_keys
// (the parser strips it), so we settle for proving the binding lands
// when the flag is set — a regression where the boundary swapped `-r`
// for a positional KEY would either hard-error from tmux or land in
// the wrong slot.
func TestHandle_BindKey_RepeatableFlag(t *testing.T) {
	t.Parallel()
	tools, call, _ := bindKeyTestSetup(t)

	const key = "F10"
	const cmd = "display-message repeatable-via-mcp"
	bound, _, _ := decodeBindKey(t, call("bind_key", map[string]any{
		"key":        key,
		"command":    cmd,
		"key_table":  "copy-mode",
		"repeatable": true,
	}))
	if !bound {
		t.Fatal("bound=false, want true")
	}
	listing := tmuxRunBindKey(t, tools.Ctl.Socket(), "list-keys", "-T", "copy-mode")
	if !strings.Contains(listing, key) {
		t.Fatalf("copy-mode listing missing %q; got\n%s", key, listing)
	}
}

// TestHandle_BindKey_RejectsMissingKey pins the schema's `required:
// ["key", "command"]` contract for the key half: without `key` the
// call must fail with CodeInvalidParams (-32602) before any tmux
// command runs.
func TestHandle_BindKey_RejectsMissingKey(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "bind_key",
		"arguments": map[string]any{
			"command": "display-message x",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing key")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_BindKey_RejectsMissingCommand is the symmetrical guard for
// the command half of `required: ["key", "command"]`.
func TestHandle_BindKey_RejectsMissingCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "bind_key",
		"arguments": map[string]any{
			"key": "F1",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_BindKey_RejectsControlByteInCommand pins the NUL/control-
// byte guard on the `command` argument. A NUL byte would hard-stop
// tmux's argv parsing and almost certainly indicates an encoding bug
// at the caller — we surface that as CodeInvalidParams up front rather
// than letting the boundary fork tmux only to fail in a noisy way.
func TestHandle_BindKey_RejectsControlByteInCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "bind_key",
		"arguments": map[string]any{
			"key":     "F1",
			"command": "display-message hello\x00world",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for NUL byte in command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "control byte") {
		t.Errorf("error msg = %q, expected to mention 'control byte'", rerr.Message)
	}
}

// TestHandle_BindKey_RejectsOversizedCommand enforces the 4 KiB cap on
// the command argument. An oversized payload must fail with
// CodeInvalidParams before any tmux process is spawned.
func TestHandle_BindKey_RejectsOversizedCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	oversized := strings.Repeat("a", maxBindKeyCommandLen+1)
	params := mustJSON(t, map[string]any{
		"name": "bind_key",
		"arguments": map[string]any{
			"key":     "F1",
			"command": oversized,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_BindKey_RejectsBadKeyTable locks the regex check on
// `key_table`: a stray space must surface as CodeInvalidParams via
// the validateKeyTable helper shared with list_keys, before tmux is
// asked to resolve the table.
func TestHandle_BindKey_RejectsBadKeyTable(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "bind_key",
		"arguments": map[string]any{
			"key":       "F1",
			"command":   "display-message x",
			"key_table": "bad table with spaces",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad key_table")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_BindKey_AdditionalPropertiesLocked enforces the
// additionalProperties:false contract on the schema — a typo like
// "table" instead of "key_table" must be visible in the schema rather
// than silently swallowed at decode time. We assert on the schema
// entry directly because the handler's typed struct already drops
// unknown JSON fields.
func TestHandle_BindKey_AdditionalPropertiesLocked(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "bind_key" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("bind_key schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		required, _ := schema["required"].([]string)
		wantReq := map[string]bool{"key": false, "command": false}
		for _, r := range required {
			if _, ok := wantReq[r]; ok {
				wantReq[r] = true
			}
		}
		for k, seen := range wantReq {
			if !seen {
				t.Errorf("bind_key schema required missing %q", k)
			}
		}
		return
	}
	t.Fatal("tools/list missing bind_key")
}

// TestHandle_ToolsList_IncludesBindKey makes sure tools/list advertises
// the new tool so MCP clients can discover it via the schema endpoint.
// Mirrors the smoke check every other tool ships with — a regression in
// init() registration would otherwise hide the tool from the surface
// even though the dispatcher case still works for a hardcoded call.
func TestHandle_ToolsList_IncludesBindKey(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "bind_key" {
			return
		}
	}
	t.Fatal("tools/list missing bind_key")
}

// TestHandle_BindKey_NotInReadOnlyAllowlist pins bind_key as a
// mutating tool. The read-only mode allowlist is the policy gate that
// stops a write tool from running under -read-only; if a future
// contributor accidentally adds bind_key to readOnlyTools the agent
// would silently regain a write surface. Mirrors the exclusion in
// readonly_test.go's RejectsMutators table.
func TestHandle_BindKey_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("bind_key") {
		t.Fatal("bind_key must not be inspection-allowed under -read-only")
	}
}

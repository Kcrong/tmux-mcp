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

// tmuxRunSourceBuffer shells out to the tmux binary on PATH against the
// supplied socket and returns stdout. Used by the source_buffer suite
// to seed paste buffers / probe support without depending on the
// list_buffers / show_buffer MCP tools (which round-trip through the
// dispatcher and would entangle the assertions). Failure aborts the
// test with a stderr-bearing message so a flaky tmux build does not
// turn into a head-scratching assertion miss.
func tmuxRunSourceBuffer(t *testing.T, socket string, args ...string) (string, string, error) {
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
	runErr := cmd.Run()
	return stdout.String(), stderr.String(), runErr
}

// skipIfNoSourceBufferTool skips the test when the tmux on PATH does
// not implement the `source-buffer` command. tmux added source-buffer
// in 3.5; older releases (the 3.4 still shipping in Ubuntu 24.04 /
// Debian stable as of 2026) emit "unknown command: source-buffer"
// instead. Probing via list-commands keeps the gate independent of
// any future rename of the verb.
func skipIfNoSourceBufferTool(t *testing.T, tools *Tools) {
	t.Helper()
	out, _, err := tmuxRunSourceBuffer(t, tools.Ctl.Socket(), "list-commands")
	if err != nil {
		t.Fatalf("list-commands probe: %v", err)
	}
	if !strings.Contains(out, "source-buffer") {
		t.Skipf("tmux on PATH does not implement source-buffer (added in 3.5)")
	}
}

// sourceBufferTestSetup spins up a fresh *Tools with an anchor session
// (so the tmux server is definitely running — buffers live on the
// server, and source-buffer against a server-less socket reports
// "error connecting" rather than executing the body), and returns a
// pre-bound `call` helper plus the deadline context. Pulling the
// boilerplate into a helper keeps every test case focused on the
// assertion that actually matters.
//
// Each caller must invoke t.Parallel() itself — t.Helper() inside a
// helper does not propagate t.Parallel, and the user-facing
// concurrency contract is "one tmux server per top-level test".
func sourceBufferTestSetup(t *testing.T) (
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
	// buffers live on the server and source-buffer requires it to be
	// running for the parser to receive any input.
	call("session_create", map[string]any{
		"name": "anchor_src_buf", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "anchor_src_buf"},
			}))
	})
	return tools, call, ctx
}

// decodeSourceBuffer pulls the {"sourced": ..., "name": ...} envelope
// out of the tools/call result so the assertions in each test stay
// focused on the field that matters.
func decodeSourceBuffer(t *testing.T, result any) (sourced bool, name string) {
	t.Helper()
	body := extractText(t, result)
	var obj struct {
		Sourced bool   `json:"sourced"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode source_buffer body %q: %v", body, err)
	}
	return obj.Sourced, obj.Name
}

// TestHandle_SourceBuffer_NamedAppliesCommands drives the load-bearing
// happy path end-to-end through the dispatcher: stash a tmux command
// line in a paste buffer, source it by name, and confirm tmux's
// options table reflects the result. status-keys defaults to "emacs"
// on a fresh server, so a later "vi" reading proves the buffer body
// actually flowed through the parser.
func TestHandle_SourceBuffer_NamedAppliesCommands(t *testing.T) {
	t.Parallel()
	tools, call, _ := sourceBufferTestSetup(t)
	skipIfNoSourceBufferTool(t, tools)

	// Seed via the existing set_buffer tool so the test exercises the
	// full MCP boundary; load_buffer would also work but adds an
	// unnecessary disk round-trip.
	call("set_buffer", map[string]any{
		"data": "set -g status-keys vi",
		"name": "cfg",
	})
	sourced, echoed := decodeSourceBuffer(t, call("source_buffer", map[string]any{
		"name": "cfg",
	}))
	if !sourced {
		t.Fatalf("source_buffer sourced=false, want true")
	}
	if echoed != "cfg" {
		t.Errorf("echoed name = %q, want %q", echoed, "cfg")
	}

	// Confirm via show_options that the body actually applied.
	res := call("show_options", map[string]any{"scope": "server"})
	body := extractText(t, res)
	var opts struct {
		Options map[string]string `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &opts); err != nil {
		t.Fatalf("decode show_options: %v\nbody=%s", err, body)
	}
	if got := opts.Options["status-keys"]; got != "vi" {
		t.Errorf("status-keys = %q, want %q (source_buffer body did not apply)", got, "vi")
	}
}

// TestHandle_SourceBuffer_DefaultPicksMostRecent pins the empty-name
// path through the dispatcher: omitting `name` (or passing "") must
// pick the most-recently-added buffer, mirroring tmux's CLI default.
// We seed two buffers in order and confirm the second one's body
// (status-keys vi) is the value tmux ends up with.
func TestHandle_SourceBuffer_DefaultPicksMostRecent(t *testing.T) {
	t.Parallel()
	tools, call, _ := sourceBufferTestSetup(t)
	skipIfNoSourceBufferTool(t, tools)

	call("set_buffer", map[string]any{
		"data": "set -g status-keys emacs",
		"name": "older",
	})
	call("set_buffer", map[string]any{
		"data": "set -g status-keys vi",
		"name": "newer",
	})
	// Two equivalent forms: explicit empty-string name, and no name
	// field at all. Both must resolve to the same buffer so the schema
	// stays back-compat with clients that omit the field entirely.
	for _, args := range []map[string]any{
		{},
		{"name": ""},
	} {
		sourced, echoed := decodeSourceBuffer(t, call("source_buffer", args))
		if !sourced {
			t.Fatalf("source_buffer sourced=false, want true (args=%v)", args)
		}
		if echoed != "" {
			t.Errorf("default echoed name = %q, want empty (args=%v)", echoed, args)
		}
	}

	res := call("show_options", map[string]any{"scope": "server"})
	body := extractText(t, res)
	var opts struct {
		Options map[string]string `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &opts); err != nil {
		t.Fatalf("decode show_options: %v\nbody=%s", err, body)
	}
	if got := opts.Options["status-keys"]; got != "vi" {
		t.Errorf("status-keys = %q, want %q (default did not pick most-recent buffer)", got, "vi")
	}
}

// TestHandle_SourceBuffer_MissingMapsCode pins the wire contract for
// an unknown buffer name: the JSON-RPC error code must surface as
// CodeSessionNotFound (-32000) so MCP clients can branch on a stable
// code rather than the (version-specific) tmux stderr text.
func TestHandle_SourceBuffer_MissingMapsCode(t *testing.T) {
	t.Parallel()
	tools, _, ctx := sourceBufferTestSetup(t)
	skipIfNoSourceBufferTool(t, tools)

	params := mustJSON(t, map[string]any{
		"name":      "source_buffer",
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

// TestHandle_SourceBuffer_RejectsBadName locks the regex check on
// `name` so a stray quote/whitespace can't slip through to the tmux
// argv. The check runs before any tmux command, so the error must
// carry CodeInvalidParams (-32602) — independent of whether the tmux
// on PATH supports source-buffer at all.
func TestHandle_SourceBuffer_RejectsBadName(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "source_buffer",
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

// TestHandle_SourceBuffer_RejectsUnknownField pins the
// `additionalProperties: false` half of the schema: any unknown field
// on `arguments` must fail with CodeInvalidParams. The struct decoder
// itself is permissive (extra fields silently ignored), so the guard
// here is the schema we hand to clients via tools/list — clients that
// validate against it will see the unknown field rejected upstream.
// We pin the schema shape directly so a future contributor cannot
// drop the strict-decode flag without this test failing.
func TestHandle_SourceBuffer_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "source_buffer" {
			schema, ok := def["inputSchema"].(map[string]any)
			if !ok {
				t.Fatalf("source_buffer inputSchema missing or wrong type: %#v", def["inputSchema"])
			}
			if got := schema["additionalProperties"]; got != false {
				t.Fatalf("source_buffer additionalProperties = %v, want false", got)
			}
			// Required field list must NOT include `name` — the docs
			// promise the call works against an empty body.
			if req, ok := schema["required"]; ok {
				if names, ok := req.([]string); ok {
					for _, n := range names {
						if n == "name" {
							t.Fatalf("source_buffer must not require `name`: schema = %#v", schema)
						}
					}
				}
			}
			return
		}
	}
	t.Fatalf("tools/list missing source_buffer")
}

// TestHandle_SourceBuffer_MalformedBodyMapsToInternal pins the
// parse-error contract: when the buffer body contains text tmux's
// command parser rejects, the failure surfaces as CodeInternal — not
// CodeSessionNotFound. Those are user-input mistakes against the
// command parser, and conflating them with the missing-buffer code
// would corrupt the wire contract for "the named thing does not
// exist".
func TestHandle_SourceBuffer_MalformedBodyMapsToInternal(t *testing.T) {
	t.Parallel()
	tools, call, ctx := sourceBufferTestSetup(t)
	skipIfNoSourceBufferTool(t, tools)

	call("set_buffer", map[string]any{
		"data": "not-a-tmux-command",
		"name": "bad_cfg",
	})
	params := mustJSON(t, map[string]any{
		"name":      "source_buffer",
		"arguments": map[string]any{"name": "bad_cfg"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for malformed buffer body, got result %#v", res)
	}
	if rerr.Code != errs.CodeInternal {
		t.Fatalf("code = %d, want CodeInternal (%d), msg=%q",
			rerr.Code, errs.CodeInternal, rerr.Message)
	}
	if rerr.Code == errs.CodeSessionNotFound {
		t.Fatalf("parse error must NOT surface as CodeSessionNotFound (msg=%q)", rerr.Message)
	}
}

// TestHandle_ToolsList_IncludesSourceBuffer makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. The schema check above (TestHandle_SourceBuffer_RejectsUnknownField)
// goes deeper; this test is the minimal "is the entry there" probe so
// a regression dropping the registration entirely fails fast.
func TestHandle_ToolsList_IncludesSourceBuffer(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "source_buffer" {
			return
		}
	}
	t.Fatalf("tools/list missing source_buffer")
}

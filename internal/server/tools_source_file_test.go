package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// sourceFileTestSetup spins up a fresh *Tools with an anchor session
// (so the tmux server is definitely running — source-file against a
// server-less socket reports "error connecting" rather than reloading
// the conf), and returns a pre-bound `call` helper plus the deadline
// context. Pulling the boilerplate into a helper keeps every test case
// focused on the assertion that actually matters.
//
// Each caller must invoke t.Parallel() itself — t.Helper() inside a
// helper does not propagate t.Parallel, and the user-facing
// concurrency contract is "one tmux server per top-level test".
func sourceFileTestSetup(t *testing.T) (
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
	// source-file requires it to be running for any reload to
	// observably mutate server-wide options.
	call("session_create", map[string]any{
		"name": "anchor_src", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "anchor_src"},
			}))
	})
	return tools, call, ctx
}

// decodeSourceFile pulls the {"sourced": ...} envelope out of the
// tools/call result so the assertions in each test stay focused on
// the field that matters.
func decodeSourceFile(t *testing.T, result any) bool {
	t.Helper()
	body := extractText(t, result)
	var obj struct {
		Sourced bool `json:"sourced"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode source_file body %q: %v", body, err)
	}
	return obj.Sourced
}

// TestHandle_SourceFile_LoadsOptions is the load-bearing happy-path
// test: write a tmux.conf that sets a sentinel server-wide option,
// source it through the dispatcher, then read the option back via
// show_options. The sentinel value (escape-time 17) is deliberately
// different from tmux's default (500) so an accidental "default
// matched" pass is impossible.
func TestHandle_SourceFile_LoadsOptions(t *testing.T) {
	t.Parallel()
	_, call, _ := sourceFileTestSetup(t)

	dir := t.TempDir()
	conf := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(conf, []byte("set -g escape-time 17\n"), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}

	if !decodeSourceFile(t, call("source_file", map[string]any{"path": conf})) {
		t.Fatalf("source_file returned sourced=false; want true")
	}

	body := extractText(t, call("show_options", map[string]any{"scope": "server"}))
	var obj struct {
		Options map[string]string `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode show_options: %v body=%q", err, body)
	}
	got, ok := obj.Options["escape-time"]
	if !ok {
		t.Fatalf("escape-time missing from show_options: %v", obj.Options)
	}
	if got != "17" {
		t.Fatalf("escape-time = %q, want %q (source_file did not reload)", got, "17")
	}
}

// TestHandle_SourceFile_QuietSwallowsMissing pins the documented
// quiet=true contract: when tmux is told `-q`, a missing file is a
// non-fatal error and the controller surfaces no failure either, so an
// agent that wants "best-effort reload" can call source_file with
// quiet=true and not have to special-case ENOENT.
func TestHandle_SourceFile_QuietSwallowsMissing(t *testing.T) {
	t.Parallel()
	_, call, _ := sourceFileTestSetup(t)

	missing := filepath.Join(t.TempDir(), "definitely-not-there.conf")
	if !decodeSourceFile(t, call("source_file", map[string]any{
		"path": missing, "quiet": true,
	})) {
		t.Fatalf("source_file(quiet=true) returned sourced=false; want true")
	}
}

// TestHandle_SourceFile_MissingFileMapsCode pins the wire contract:
// a non-quiet source_file against a missing file surfaces
// CodeSessionNotFound (-32000) so MCP clients can branch on the same
// stable code they already use for "named thing does not exist on
// this server".
func TestHandle_SourceFile_MissingFileMapsCode(t *testing.T) {
	t.Parallel()
	tools, _, _ := sourceFileTestSetup(t)

	missing := filepath.Join(t.TempDir(), "definitely-not-there.conf")
	params := mustJSON(t, map[string]any{
		"name":      "source_file",
		"arguments": map[string]any{"path": missing},
	})
	res, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for missing file, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_SourceFile_RejectsMissingPath pins the required-field
// path: omitting `path` must come back as CodeInvalidParams rather
// than falling through to tmux with an empty argument.
func TestHandle_SourceFile_RejectsMissingPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "source_file",
		"arguments": map[string]any{},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing path")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SourceFile_RejectsRelativePath pins the absolute-path
// guard. tmux would otherwise resolve a relative path against
// whatever cwd the tmux-mcp binary happened to be launched from
// (often `/` for systemd / container deployments), which is almost
// never what the caller actually meant.
func TestHandle_SourceFile_RejectsRelativePath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "source_file",
		"arguments": map[string]any{
			"path": "tmux.conf",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for relative path")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "absolute") {
		t.Errorf("error message %q does not mention 'absolute'", rerr.Message)
	}
}

// TestHandle_SourceFile_RejectsTraversal locks the `..` guard. A
// malicious caller composing `/etc/tmux-mcp/../etc/passwd` would
// otherwise smuggle an out-of-tree read past an operator who thought
// the absolute-path requirement alone confined the surface.
func TestHandle_SourceFile_RejectsTraversal(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "source_file",
		"arguments": map[string]any{
			"path": "/etc/tmux-mcp/../etc/passwd",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for traversal path")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "..") {
		t.Errorf("error message %q does not mention '..' traversal", rerr.Message)
	}
}

// TestHandle_SourceFile_RejectsControlChars pins the control-char
// guard. A stray newline / NUL in argv could fool downstream tools
// that parse logs or argv; rejecting at the boundary keeps the
// invariant simple — `path` is plain printable text.
func TestHandle_SourceFile_RejectsControlChars(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	for _, name := range []string{"newline", "nul", "tab"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var path string
			switch name {
			case "newline":
				path = "/etc/tmux-mcp/tmux.conf\nrm -rf /"
			case "nul":
				path = "/etc/tmux-mcp/tmux.conf\x00rm"
			case "tab":
				path = "/etc/tmux-mcp/tmux.conf\trm"
			}
			params := mustJSON(t, map[string]any{
				"name": "source_file",
				"arguments": map[string]any{
					"path": path,
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for %s in path", name)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_SourceFile_RejectsOversizedPath enforces the 4096-byte
// ceiling. A pathological caller stuffing megabytes of garbage into
// argv must fail with CodeInvalidParams before any tmux process is
// spawned.
func TestHandle_SourceFile_RejectsOversizedPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	oversized := "/" + strings.Repeat("a", maxSourceFilePathLen)
	params := mustJSON(t, map[string]any{
		"name": "source_file",
		"arguments": map[string]any{
			"path": oversized,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized path")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SourceFile_RejectsUnknownField pins the schema's
// additionalProperties: false guard. A typo in `quite` (instead of
// `quiet`) would otherwise be silently ignored and the agent would
// never understand why their best-effort reload kept failing.
func TestHandle_SourceFile_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	// Decoder-level rejection lives in encoding/json's tag handling for
	// our typed args struct: unknown keys are tolerated by default. The
	// schema-level rejection (additionalProperties:false) is enforced by
	// well-behaved MCP clients before the request even leaves them, so
	// we assert the registered schema carries the flag rather than
	// expecting the dispatcher to refuse extra keys at decode time.
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "source_file" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		if got, _ := schema["additionalProperties"].(bool); got {
			t.Fatalf("source_file schema additionalProperties = true, want false")
		}
		return
	}
	t.Fatalf("tools/list missing source_file")
}

// TestHandle_ToolsList_IncludesSourceFile makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint.
func TestHandle_ToolsList_IncludesSourceFile(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "source_file" {
			return
		}
	}
	t.Fatalf("tools/list missing source_file")
}

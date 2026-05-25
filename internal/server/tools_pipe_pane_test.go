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

// TestHandle_PipePane_LogsOutputToFile drives the happy path end-to-end
// through the dispatcher: session_create → pipe_pane → send_keys to
// emit a sentinel → assert the operator-supplied log file picked up the
// bytes. Verifies the dispatcher is wired up, the schema accepts the
// documented arguments, and the response envelope carries the
// `{"piped": true}` ack.
func TestHandle_PipePane_LogsOutputToFile(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	call := func(name string, args any) any {
		t.Helper()
		params := mustJSON(t, map[string]any{"name": name, "arguments": args})
		res, rerr := tools.Handle(ctx, "tools/call", params)
		if rerr != nil {
			t.Fatalf("%s: %s", name, rerr.Message)
		}
		return res
	}

	call("session_create", map[string]any{
		"name": "pp", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "pp"}}))
	})

	out := filepath.Join(t.TempDir(), "pipe.log")
	pipeText := extractText(t, call("pipe_pane", map[string]any{
		"target":        "pp",
		"shell_command": "cat > " + out,
	}))
	var pipeObj struct {
		Piped bool `json:"piped"`
	}
	if err := json.Unmarshal([]byte(pipeText), &pipeObj); err != nil {
		t.Fatalf("decode pipe_pane: %v\nbody=%s", err, pipeText)
	}
	if !pipeObj.Piped {
		t.Fatalf("pipe_pane.piped = false, want true; body=%s", pipeText)
	}

	call("send_keys", map[string]any{
		"session": "pp",
		"keys":    []string{"echo PIPE_HANDLER_HELLO_42", "Enter"},
	})
	call("wait_for_text", map[string]any{
		"session": "pp", "pattern": "PIPE_HANDLER_HELLO_42", "timeout_ms": 8000,
	})

	// Pipe is async: tmux flushes captured output through `cat` on its
	// own schedule. Poll the file until the sentinel appears or the
	// deadline trips.
	deadline := time.Now().Add(5 * time.Second)
	var body []byte
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(out) //nolint:gosec // path is t.TempDir-rooted, controlled by the test
		if err == nil && strings.Contains(string(b), "PIPE_HANDLER_HELLO_42") {
			body = b
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(string(body), "PIPE_HANDLER_HELLO_42") {
		t.Fatalf("pipe log %q never picked up the sentinel; body=%q", out, body)
	}

	// Stop the pipe. Empty shell_command is the documented "tear down"
	// form; the second call must succeed and return the same ack shape.
	stopText := extractText(t, call("pipe_pane", map[string]any{
		"target": "pp",
	}))
	var stopObj struct {
		Piped bool `json:"piped"`
	}
	if err := json.Unmarshal([]byte(stopText), &stopObj); err != nil {
		t.Fatalf("decode pipe_pane(stop): %v\nbody=%s", err, stopText)
	}
	if !stopObj.Piped {
		t.Fatalf("pipe_pane(stop).piped = false, want true; body=%s", stopText)
	}
}

// TestHandle_PipePane_RejectsMissingTarget pins the required-field path:
// omitting `target` must come back as CodeInvalidParams rather than
// falling through to tmux with an empty -t value (which tmux would
// resolve to whatever pane it considers current).
func TestHandle_PipePane_RejectsMissingTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "pipe_pane",
		"arguments": map[string]any{},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PipePane_RejectsBadTarget locks the regex check on
// `target` so a stray quote/whitespace/path-injection can't slip
// through to the tmux argv.
func TestHandle_PipePane_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pipe_pane",
		"arguments": map[string]any{
			"target":        "demo:0.0;rm -rf /",
			"shell_command": "cat > /dev/null",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PipePane_RejectsControlChars locks the boundary policy on
// `shell_command`: a NUL byte or other control character (newline, ESC,
// …) must be rejected before any tmux call runs. The shell command is
// passed verbatim to /bin/sh by tmux, so we are responsible for
// ensuring the bytes that get there can't smuggle past the JSON-RPC
// frame or tmux's own argv parser.
func TestHandle_PipePane_RejectsControlChars(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	cases := []struct {
		name string
		cmd  string
	}{
		{"NUL byte", "cat > /tmp/x\x00.log"},
		{"newline", "cat > /tmp/x.log\necho boom"},
		{"escape", "echo \x1b[31mred"},
		{"DEL", "echo a\x7fb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "pipe_pane",
				"arguments": map[string]any{
					"target":        "pp",
					"shell_command": tc.cmd,
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for %s", tc.name)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_PipePane_RejectsOversizeCommand pins the 4 KiB ceiling on
// shell_command — the boundary refuses oversized payloads up front so a
// hostile or buggy caller can't pin tmux on a multi-megabyte argv.
func TestHandle_PipePane_RejectsOversizeCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	huge := strings.Repeat("a", maxPipePaneShellCommandLen+1)
	params := mustJSON(t, map[string]any{
		"name": "pipe_pane",
		"arguments": map[string]any{
			"target":        "pp",
			"shell_command": huge,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversize shell_command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PipePane_MissingSessionMapsCode pins the wire contract that
// pipe_pane against a target on an unknown session surfaces
// CodeSessionNotFound (-32000), mirroring pane_kill / pane_select /
// clear_history.
func TestHandle_PipePane_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, pane missing"
	// rather than "no server" (different stderr shape).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name": "pipe_pane",
		"arguments": map[string]any{
			"target":        "definitely_does_not_exist_xyzzy:0.0",
			"shell_command": "cat > /dev/null",
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_ToolsList_IncludesPipePane makes sure tools/list advertises
// the new tool so MCP clients can discover it via the schema endpoint.
func TestHandle_ToolsList_IncludesPipePane(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "pipe_pane" {
			return
		}
	}
	t.Fatalf("tools/list missing pipe_pane")
}

// TestHandle_PipePane_RejectsWhitespaceOnly pins the defensive guard:
// a `shell_command` that is non-empty but only whitespace is rejected as
// CodeInvalidParams rather than starting an effectively-no-op pipe.
func TestHandle_PipePane_RejectsWhitespaceOnly(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pipe_pane",
		"arguments": map[string]any{
			"target":        "pp",
			"shell_command": "   \t  ",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for whitespace-only shell_command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

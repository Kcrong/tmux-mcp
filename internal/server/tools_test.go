package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

func skipIfNoTmux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("tmux tests require unix-like OS")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
}

func newTools(t *testing.T) *Tools {
	t.Helper()
	c, err := tmuxctl.New()
	if err != nil {
		t.Fatalf("tmuxctl.New: %v", err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })
	return NewTools(c)
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// extractText pulls the text from a tool-call result that uses the
// {"content":[{"type":"text","text":"..."}]} shape.
func extractText(t *testing.T, result any) string {
	t.Helper()
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %#v", result)
	}
	content, ok := m["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content not in expected shape: %#v", m)
	}
	if len(content) == 0 {
		return ""
	}
	return content[0]["text"].(string)
}

func TestHandle_InitializeAndList(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx := context.Background()

	res, rerr := tools.Handle(ctx, "initialize", nil)
	if rerr != nil {
		t.Fatalf("initialize: %v", rerr)
	}
	m := res.(map[string]any)
	if m["protocolVersion"] != "2024-11-05" {
		t.Fatalf("protocol: %v", m["protocolVersion"])
	}

	res, rerr = tools.Handle(ctx, "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	if len(listing) < 5 {
		t.Fatalf("expected several tools listed, got %d", len(listing))
	}
	wanted := map[string]bool{
		"session_create": false, "session_list": false, "session_kill": false,
		"send_keys": false, "capture": false, "wait_for_stable": false,
		"wait_for_text": false, "snapshot_diff": false, "resize": false,
	}
	for _, def := range listing {
		name := def["name"].(string)
		if _, ok := wanted[name]; ok {
			wanted[name] = true
		}
	}
	for n, ok := range wanted {
		if !ok {
			t.Errorf("tools/list missing %q", n)
		}
	}
}

func TestHandle_SessionRoundTrip(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

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
		"name": "rt", "command": "/bin/sh", "width": 80, "height": 20,
	})

	listText := extractText(t, call("session_list", map[string]any{}))
	if !strings.Contains(listText, `"rt"`) {
		t.Fatalf("session_list missing rt: %s", listText)
	}

	call("send_keys", map[string]any{
		"session": "rt",
		"keys":    []string{"echo HELLO_RT_TEST", "Enter"},
	})

	stableText := extractText(t, call("wait_for_stable", map[string]any{
		"session": "rt", "quiet_ms": 250, "timeout_ms": 4000,
	}))
	if !strings.Contains(stableText, "HELLO_RT_TEST") {
		t.Fatalf("wait_for_stable did not see sentinel: %s", stableText)
	}

	captureText := extractText(t, call("capture", map[string]any{"session": "rt"}))
	if !strings.Contains(captureText, "HELLO_RT_TEST") {
		t.Fatalf("capture did not see sentinel: %s", captureText)
	}

	matchText := extractText(t, call("wait_for_text", map[string]any{
		"session": "rt", "pattern": `HELLO_RT_TEST`, "timeout_ms": 3000,
	}))
	if !strings.Contains(matchText, "HELLO_RT_TEST") {
		t.Fatalf("wait_for_text did not match: %s", matchText)
	}

	call("session_kill", map[string]any{"name": "rt"})
}

func TestHandle_SnapshotDiff(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

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
		"name": "sd", "command": "/bin/sh", "width": 80, "height": 20,
	})

	// First diff against an empty token → expect everything as new.
	first := extractText(t, call("snapshot_diff", map[string]any{
		"session": "sd", "prior_token": "",
	}))
	var firstObj map[string]any
	if err := json.Unmarshal([]byte(first), &firstObj); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	tok, _ := firstObj["token"].(string)
	if tok == "" {
		t.Fatal("first snapshot_diff returned empty token")
	}

	// Drive a change.
	call("send_keys", map[string]any{
		"session": "sd", "keys": []string{"echo TICK", "Enter"},
	})
	_ = extractText(t, call("wait_for_stable", map[string]any{
		"session": "sd", "quiet_ms": 200, "timeout_ms": 4000,
	}))

	// Second diff using the prior token → at least one new line.
	second := extractText(t, call("snapshot_diff", map[string]any{
		"session": "sd", "prior_token": tok,
	}))
	var secondObj map[string]any
	if err := json.Unmarshal([]byte(second), &secondObj); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	diffs, _ := secondObj["diff"].([]any)
	if len(diffs) == 0 {
		t.Fatalf("expected non-empty diff, got %s", second)
	}
}

func TestHandle_SessionKillForgetsSnapshotHistory(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

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
		"name": "kf", "command": "/bin/sh", "width": 80, "height": 20,
	})

	// Populate snapshot history for the session.
	_ = extractText(t, call("capture", map[string]any{"session": "kf"}))
	if !tools.Snap.Has("kf") {
		t.Fatal("expected snapshot history for kf after capture")
	}

	call("session_kill", map[string]any{"name": "kf"})

	if tools.Snap.Has("kf") {
		t.Fatal("session_kill should have forgotten snapshot history for kf")
	}
}

func TestCapCaptureBody(t *testing.T) {
	mkLines := func(n int) string {
		parts := make([]string, n)
		for i := 0; i < n; i++ {
			parts[i] = fmt.Sprintf("line-%d", i)
		}
		return strings.Join(parts, "\n")
	}

	t.Run("visible no cap when max_lines=0", func(t *testing.T) {
		body := mkLines(200)
		got, truncated := capCaptureBody(body, tmuxctl.CaptureVisible, 0)
		if truncated {
			t.Fatalf("visible mode without max_lines should not truncate")
		}
		if got != body {
			t.Fatalf("visible body modified unexpectedly")
		}
	})

	t.Run("visible respects max_lines", func(t *testing.T) {
		body := mkLines(200)
		got, truncated := capCaptureBody(body, tmuxctl.CaptureVisible, 50)
		if !truncated {
			t.Fatalf("expected truncation when max_lines<lines")
		}
		gotLines := strings.Split(got, "\n")
		if len(gotLines) != 50 {
			t.Fatalf("expected 50 lines, got %d", len(gotLines))
		}
		if gotLines[len(gotLines)-1] != "line-199" {
			t.Fatalf("expected newest line preserved, got %q", gotLines[len(gotLines)-1])
		}
		if gotLines[0] != "line-150" {
			t.Fatalf("expected oldest line dropped, got first %q", gotLines[0])
		}
	})

	t.Run("scrollback applies default cap when max_lines=0", func(t *testing.T) {
		body := mkLines(6000)
		got, truncated := capCaptureBody(body, tmuxctl.CaptureScrollback, 0)
		if !truncated {
			t.Fatalf("expected default scrollback cap to truncate")
		}
		gotLines := strings.Split(got, "\n")
		if len(gotLines) != defaultScrollbackMaxLines {
			t.Fatalf("expected %d lines, got %d", defaultScrollbackMaxLines, len(gotLines))
		}
		if gotLines[len(gotLines)-1] != "line-5999" {
			t.Fatalf("expected newest line preserved, got %q", gotLines[len(gotLines)-1])
		}
	})

	t.Run("scrollback respects explicit max_lines", func(t *testing.T) {
		body := mkLines(1000)
		got, truncated := capCaptureBody(body, tmuxctl.CaptureScrollback, 100)
		if !truncated {
			t.Fatalf("expected truncation with explicit cap")
		}
		gotLines := strings.Split(got, "\n")
		if len(gotLines) != 100 {
			t.Fatalf("expected 100 lines, got %d", len(gotLines))
		}
	})

	t.Run("scrollback short body unchanged", func(t *testing.T) {
		body := mkLines(10)
		got, truncated := capCaptureBody(body, tmuxctl.CaptureScrollback, 0)
		if truncated {
			t.Fatalf("did not expect truncation for body shorter than cap")
		}
		if got != body {
			t.Fatalf("body modified unexpectedly")
		}
	})
}

func TestCaptureHandler_ScrollbackTruncated(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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
		"name": "cap", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "cap"}}))
	})

	// Pump a few hundred lines into the scrollback. The integration test
	// exercises the end-to-end path; the comprehensive 6000→5000 default
	// cap check lives in TestCapCaptureBody where we can control the
	// input precisely (tmux's own history-limit caps what we can stuff
	// through send-keys here).
	call("send_keys", map[string]any{
		"session": "cap",
		"keys":    []string{"seq 1 500", "Enter"},
	})
	_ = extractText(t, call("wait_for_stable", map[string]any{
		"session": "cap", "quiet_ms": 400, "timeout_ms": 8000,
	}))

	// Force truncation with an explicit small cap so the assertion is
	// independent of tmux's history-limit.
	captureText := extractText(t, call("capture", map[string]any{
		"session":   "cap",
		"mode":      "scrollback",
		"max_lines": 50,
	}))
	var capObj map[string]any
	if err := json.Unmarshal([]byte(captureText), &capObj); err != nil {
		t.Fatalf("decode capture: %v\nbody=%s", err, captureText)
	}

	body, _ := capObj["snapshot"].(string)
	lines := strings.Split(body, "\n")
	if len(lines) > 50 {
		t.Fatalf("snapshot exceeds requested cap: got %d lines, want <= 50", len(lines))
	}
	if truncated, _ := capObj["truncated"].(bool); !truncated {
		t.Fatalf("expected truncated=true when cap forces truncation")
	}
}

func TestCaptureHandler_VisibleBackcompat(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

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
		"name": "vis", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "vis"}}))
	})

	call("send_keys", map[string]any{
		"session": "vis", "keys": []string{"echo HELLO_VIS", "Enter"},
	})
	_ = extractText(t, call("wait_for_stable", map[string]any{
		"session": "vis", "quiet_ms": 250, "timeout_ms": 4000,
	}))

	captureText := extractText(t, call("capture", map[string]any{"session": "vis"}))
	var capObj map[string]any
	if err := json.Unmarshal([]byte(captureText), &capObj); err != nil {
		t.Fatalf("decode capture: %v", err)
	}
	body, _ := capObj["snapshot"].(string)
	if !strings.Contains(body, "HELLO_VIS") {
		t.Fatalf("expected sentinel in visible capture: %s", body)
	}
	if truncated, _ := capObj["truncated"].(bool); truncated {
		t.Fatalf("default visible mode should never truncate")
	}
}

func TestHandle_ToolCallUnknown(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{"name": "nonexistent_tool", "arguments": map[string]any{}})
	res, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for unknown tool, got result %#v", res)
	}
	if rerr.Code != codeMethodNotFound {
		t.Fatalf("unexpected error code: %d", rerr.Code)
	}
}

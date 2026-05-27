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

// tmuxRunPaste shells out to the tmux binary on PATH against the
// supplied socket and returns stdout. Used by the paste_buffer suite to
// seed buffers, observe the post-paste pane, and probe list-buffers
// without depending on the show_buffer / list_buffers MCP tools — those
// tools work, but the paste_buffer assertions need a path that is
// independent of the wrapped surface so a regression in the MCP layer
// can't masquerade as "the test is fine, the seeding is fine".
func tmuxRunPaste(t *testing.T, socket string, args ...string) string {
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

// pasteBufferTestSetup spins up a fresh *Tools with an anchor session
// (so the tmux server is definitely running and there is a pane to
// paste into) and returns a pre-bound `call` helper plus the
// deadline context. The session uses a stripped-down PS1 to keep the
// post-paste capture-pane assertions readable across distros that
// ship colourful default prompts. Each caller must invoke
// t.Parallel() itself — t.Helper does not propagate the parallel
// flag, and the user-facing concurrency contract is "one tmux server
// per top-level test".
func pasteBufferTestSetup(t *testing.T, sessionName string) (
	tools *Tools,
	call func(name string, args any) any,
	target string,
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
		"name": sessionName, "command": "/bin/sh", "width": 80, "height": 24,
		"env": map[string]any{"PS1": "$ "},
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": sessionName},
			}))
	})
	target = sessionName + ":0.0"
	return tools, call, target, ctx
}

// waitForPaneCapture polls capture-pane against target until the
// visible region contains marker, or the deadline expires. Asserting
// on the captured bytes — rather than on tmux's exit code from
// paste-buffer — proves the bytes actually hit the pty: a successful
// paste-buffer call against an empty buffer returns 0 too, so an
// exit-code-only assertion would silently regress.
func waitForPaneCapture(t *testing.T, socket, target, marker string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out := tmuxRunPaste(t, socket, "capture-pane", "-p", "-t", target)
		last = out
		if strings.Contains(out, marker) {
			return out
		}
		time.Sleep(75 * time.Millisecond)
	}
	t.Fatalf("marker %q never appeared in %s; last capture:\n%s", marker, target, last)
	return last
}

// TestHandle_PasteBuffer_NamedDelivers drives the explicit-name happy
// path through the dispatcher: stash a sentinel under a pinned buffer
// name, paste it into the anchored session's active pane, and confirm
// the bytes show up in the visible region. The response must be the
// documented `{"pasted": true}` ack so an MCP client can branch on a
// stable shape.
func TestHandle_PasteBuffer_NamedDelivers(t *testing.T) {
	t.Parallel()
	tools, call, target, _ := pasteBufferTestSetup(t, "pb_named")
	socket := tools.Ctl.Socket()

	const sentinel = "PASTE_HANDLER_NAMED_MARK"
	tmuxRunPaste(t, socket, "set-buffer", "-b", "handler_named", sentinel)

	body := extractText(t, call("paste_buffer", map[string]any{
		"target":      target,
		"buffer_name": "handler_named",
	}))
	var ack struct {
		Pasted bool `json:"pasted"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode paste_buffer body %q: %v", body, err)
	}
	if !ack.Pasted {
		t.Fatalf("paste_buffer pasted=false, want true (body=%s)", body)
	}

	waitForPaneCapture(t, socket, target, sentinel)

	// The buffer must still be present (delete_after defaulted to false)
	// so a follow-up paste could hit the same name. list-buffers is the
	// cheapest probe.
	listing := tmuxRunPaste(t, socket, "list-buffers", "-F", "#{buffer_name}")
	if !strings.Contains(listing, "handler_named") {
		t.Fatalf("buffer handler_named was unexpectedly gone after paste; listing=%q", listing)
	}
}

// TestHandle_PasteBuffer_DefaultPicksMostRecent locks the empty-name
// path through the dispatcher: omitting `buffer_name` resolves to the
// most-recently-added buffer. We seed two buffers so the assertion
// catches a regression that silently switched to "first" or
// "alphabetical" ordering, and we also verify the buffer survives
// (delete_after default false) so subsequent calls keep working.
func TestHandle_PasteBuffer_DefaultPicksMostRecent(t *testing.T) {
	t.Parallel()
	tools, call, target, _ := pasteBufferTestSetup(t, "pb_default")
	socket := tools.Ctl.Socket()

	tmuxRunPaste(t, socket, "set-buffer", "PASTE_HANDLER_OLDEST_MARK")
	tmuxRunPaste(t, socket, "set-buffer", "PASTE_HANDLER_NEWEST_MARK")

	body := extractText(t, call("paste_buffer", map[string]any{
		"target": target,
	}))
	var ack struct {
		Pasted bool `json:"pasted"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode paste_buffer body %q: %v", body, err)
	}
	if !ack.Pasted {
		t.Fatalf("paste_buffer pasted=false, want true (body=%s)", body)
	}

	captured := waitForPaneCapture(t, socket, target, "PASTE_HANDLER_NEWEST_MARK")
	if strings.Contains(captured, "PASTE_HANDLER_OLDEST_MARK") {
		t.Fatalf("default paste delivered the wrong buffer; capture=\n%s", captured)
	}
}

// TestHandle_PasteBuffer_DeleteAfterRemovesBuffer pins the
// delete_after=true round-trip end-to-end: after the paste lands the
// buffer must be gone from list-buffers. This is the load-bearing
// flag for an agent that wants a one-shot snippet that does not
// linger in the server's buffer table.
func TestHandle_PasteBuffer_DeleteAfterRemovesBuffer(t *testing.T) {
	t.Parallel()
	tools, call, target, _ := pasteBufferTestSetup(t, "pb_delete")
	socket := tools.Ctl.Socket()

	const sentinel = "PASTE_HANDLER_DELETE_MARK"
	tmuxRunPaste(t, socket, "set-buffer", "-b", "ephemeral_handler", sentinel)
	pre := tmuxRunPaste(t, socket, "list-buffers", "-F", "#{buffer_name}")
	if !strings.Contains(pre, "ephemeral_handler") {
		t.Fatalf("buffer ephemeral_handler missing before paste; listing=%q", pre)
	}

	body := extractText(t, call("paste_buffer", map[string]any{
		"target":       target,
		"buffer_name":  "ephemeral_handler",
		"delete_after": true,
	}))
	var ack struct {
		Pasted bool `json:"pasted"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode paste_buffer body %q: %v", body, err)
	}
	if !ack.Pasted {
		t.Fatalf("paste_buffer pasted=false, want true (body=%s)", body)
	}

	waitForPaneCapture(t, socket, target, sentinel)

	post := tmuxRunPaste(t, socket, "list-buffers", "-F", "#{buffer_name}")
	if strings.Contains(post, "ephemeral_handler") {
		t.Fatalf("delete_after=true did not drop the buffer; listing=%q", post)
	}
}

// TestHandle_PasteBuffer_MissingMapsCode pins the wire contract for an
// unknown buffer name: the JSON-RPC error code must surface as
// CodeSessionNotFound (-32000) so MCP clients can branch on a stable
// code rather than the (version-specific) tmux stderr text.
func TestHandle_PasteBuffer_MissingMapsCode(t *testing.T) {
	t.Parallel()
	tools, _, target, ctx := pasteBufferTestSetup(t, "pb_missing")

	params := mustJSON(t, map[string]any{
		"name": "paste_buffer",
		"arguments": map[string]any{
			"target":      target,
			"buffer_name": "ghost_paste_buffer_handler_xyzzy",
		},
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

// TestHandle_PasteBuffer_RejectsMissingTarget pins the up-front guard:
// without `target` the call must fail with CodeInvalidParams (-32602)
// before any tmux command runs.
func TestHandle_PasteBuffer_RejectsMissingTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "paste_buffer",
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

// TestHandle_PasteBuffer_RejectsBadTarget locks the regex check on
// `target`: a stray quote or whitespace must trip the validator
// before any tmux command runs, with CodeInvalidParams as the
// resulting error code.
func TestHandle_PasteBuffer_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "paste_buffer",
		"arguments": map[string]any{
			"target": "bad target with spaces",
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

// TestHandle_PasteBuffer_RejectsBadBufferName locks the regex check
// on `buffer_name` so a stray quote / whitespace can't slip through to
// the tmux argv. The check runs before any tmux command, so the
// error must carry CodeInvalidParams (-32602).
func TestHandle_PasteBuffer_RejectsBadBufferName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "paste_buffer",
		"arguments": map[string]any{
			"target":      "anchor:0.0",
			"buffer_name": "bad name with spaces",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad buffer_name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ToolsList_IncludesPasteBuffer makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Without this pin a future contributor dropping
// the init() registration would silently hide the tool.
func TestHandle_ToolsList_IncludesPasteBuffer(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "paste_buffer" {
			return
		}
	}
	t.Fatalf("tools/list missing paste_buffer")
}

// TestIsReadOnlyTool_PasteBufferIsMutator pins the policy that
// paste_buffer is NOT in the read-only allowlist. paste_buffer
// forwards stored bytes through tmux's paste machinery into the
// targeted pane's pty, which is observable as state mutation by any
// process reading that pty — so an agent constrained to inspection
// must not be able to invoke it. Without this pin a future
// contributor adding the tool to readonly.go would silently break
// that contract.
func TestIsReadOnlyTool_PasteBufferIsMutator(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("paste_buffer") {
		t.Fatal("IsReadOnlyTool(\"paste_buffer\") = true, want false (paste_buffer mutates pty state)")
	}
}

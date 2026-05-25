package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// TestHandle_PaneSwap_SwapsTwoPanes drives the happy path end-to-end:
// session_create → pane_split (vertical, detach) → tag each pane with a
// distinguishing sentinel via send_keys → pane_swap → capture each
// position again and assert the contents traded places. tmux's
// swap-pane keeps `#{pane_id}` glued to its buffer/process, so the test
// observes the swap by capturing through the position targets
// (`session:window.pane`) before and after.
//
// The boundary tools (capture / send_keys) only accept bare-session
// refs, so the test reaches into tools.Ctl directly to drive the
// pane-target forms tmux understands. This is intentional: the test
// is asserting the *swap* semantics, not the boundary regex (which is
// covered by the dedicated reject-bad-src/dst cases below).
func TestHandle_PaneSwap_SwapsTwoPanes(t *testing.T) {
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
		"name": "psw", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "psw"}}))
	})

	// Two panes, side-by-side. detach=true keeps focus deterministic.
	_ = extractText(t, call("pane_split", map[string]any{
		"session":   "psw",
		"direction": "vertical",
		"detach":    true,
	}))

	// Tag each pane with a distinct sentinel via the controller (whose
	// SendKeys forwards the target verbatim, no boundary regex).
	if err := tools.Ctl.SendKeys(ctx, "psw:0.0", []string{"echo PANEZERO_SWAP_TAG", "Enter"}, false); err != nil {
		t.Fatalf("SendKeys psw:0.0: %v", err)
	}
	if err := tools.Ctl.SendKeys(ctx, "psw:0.1", []string{"echo PANEONE_SWAP_TAG", "Enter"}, false); err != nil {
		t.Fatalf("SendKeys psw:0.1: %v", err)
	}

	// Wait for both panes to display their sentinel before swapping so
	// the post-swap capture isn't racing the shell's redraw.
	waitFor := func(target, want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			body, err := tools.Ctl.Capture(ctx, target, tmuxctl.CaptureVisible, false)
			if err != nil {
				t.Fatalf("Capture %s: %v", target, err)
			}
			if strings.Contains(body, want) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%q never appeared at %s", want, target)
	}
	waitFor("psw:0.0", "PANEZERO_SWAP_TAG")
	waitFor("psw:0.1", "PANEONE_SWAP_TAG")

	out := call("pane_swap", map[string]any{
		"src": "psw:0.0",
		"dst": "psw:0.1",
	})
	if got := extractText(t, out); got != "ok" {
		t.Fatalf("pane_swap = %q, want \"ok\"", got)
	}

	// Capture each position again. The sentinels must have swapped:
	// what was at psw:0.0 should now be at psw:0.1 and vice versa.
	zeroAfter, err := tools.Ctl.Capture(ctx, "psw:0.0", tmuxctl.CaptureVisible, false)
	if err != nil {
		t.Fatalf("Capture psw:0.0 after swap: %v", err)
	}
	oneAfter, err := tools.Ctl.Capture(ctx, "psw:0.1", tmuxctl.CaptureVisible, false)
	if err != nil {
		t.Fatalf("Capture psw:0.1 after swap: %v", err)
	}
	if !strings.Contains(zeroAfter, "PANEONE_SWAP_TAG") {
		t.Fatalf("after swap, psw:0.0 missing PANEONE_SWAP_TAG; body=%s", zeroAfter)
	}
	if !strings.Contains(oneAfter, "PANEZERO_SWAP_TAG") {
		t.Fatalf("after swap, psw:0.1 missing PANEZERO_SWAP_TAG; body=%s", oneAfter)
	}
}

// TestHandle_PaneSwap_RejectsEmptySrc guards the required-field path:
// the schema lists src as required, but the handler must also reject
// the empty string at runtime so a half-formed call cannot leak a
// stray "" past the regex.
func TestHandle_PaneSwap_RejectsEmptySrc(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "pane_swap",
		"arguments": map[string]any{"src": "", "dst": "demo:0.1"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty src")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneSwap_RejectsEmptyDst mirrors the src guard for the
// destination argument so tmux never sees a "-t" without a value.
func TestHandle_PaneSwap_RejectsEmptyDst(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "pane_swap",
		"arguments": map[string]any{"src": "demo:0.0", "dst": ""},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty dst")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneSwap_RejectsBadSrc locks the regex check for the src
// target — a stray quote / shell metachar must not slip through to the
// tmux argv, even though the boundary already guards `session` fields
// elsewhere.
func TestHandle_PaneSwap_RejectsBadSrc(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_swap",
		"arguments": map[string]any{
			"src": "demo:0.0;rm -rf /",
			"dst": "demo:0.1",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad src")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneSwap_MissingSessionMapsCode pins the wire contract that
// pane_swap against an unknown session surfaces CodeSessionNotFound
// (-32000), mirroring pane_select / pane_split.
func TestHandle_PaneSwap_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor so we hit "server up, session missing" rather than "no
	// server" (different stderr shape).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name": "pane_swap",
		"arguments": map[string]any{
			"src": "definitely_does_not_exist_xyzzy:0.0",
			"dst": "anchor:0.0",
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

// TestHandle_ToolsList_IncludesPaneSwap makes sure tools/list advertises
// the new tool so MCP clients can discover it via the schema endpoint.
func TestHandle_ToolsList_IncludesPaneSwap(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "pane_swap" {
			return
		}
	}
	t.Fatalf("tools/list missing pane_swap")
}

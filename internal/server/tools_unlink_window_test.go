package server

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// runTmuxLink drives a raw `tmux link-window -s <src> -t <dst>`
// through the controller's private socket so the unlink tests can
// construct multi-session windows without depending on a `link_window`
// tool that lives behind a separate PR (#109). Mirrors the
// runTmux helper tools_respawn_pane_test.go uses for the same reason
// — going straight at tmux keeps the test independent of what the
// controller API happens to wrap today.
func runTmuxLink(t *testing.T, socket, src, dst string) {
	t.Helper()
	cmd := exec.Command("tmux", "-S", socket, "link-window", "-s", src, "-t", dst) //nolint:gosec // socket is a private path produced by tmuxctl.New
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("tmux link-window -s %s -t %s: %v\nstderr=%s", src, dst, err, stderr.String())
	}
}

// TestHandle_UnlinkWindow_RemovesLinkLeavesSourceAlive runs the happy
// path through the dispatcher: a window grafted into a second session
// via `tmux link-window` is unlinked from the destination, and a
// follow-up list_windows on each side reflects the result — the
// destination drops the slot while the source keeps the original.
// Catches the dispatcher wiring, the target validator, and the
// controller argv ordering in one shot.
func TestHandle_UnlinkWindow_RemovesLinkLeavesSourceAlive(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "uwhs", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "uwhs"}}))
	})
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "uwhd", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "uwhd"}}))
	})
	// Detached create so the active flag stays put — unlink_window
	// cares about layout, not focus.
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "uwhs", "name": "shared", "command": "/bin/sh", "select": false,
	})
	// Pre-flight: graft uwhs:shared into uwhd:1 directly via tmux.
	// link_window is on a separate PR, so route through raw tmux to
	// keep this test focused on the unlink contract.
	runTmuxLink(t, tools.Ctl.Socket(), "uwhs:shared", "uwhd:1")

	got := extractText(t, callTool(t, tools, ctx, "unlink_window", map[string]any{
		"target": "uwhd:1",
	}))
	if !strings.Contains(got, `"unlinked":true`) {
		t.Fatalf("unlink_window text = %q, want it to contain 'unlinked:true'", got)
	}

	// Source side: the original "shared" window must still be present.
	srcMap := listWindowNames(t, extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "uwhs"})))
	var srcHasShared bool
	for _, name := range srcMap {
		if name == "shared" {
			srcHasShared = true
		}
	}
	if !srcHasShared {
		t.Errorf("src lost the linked window after unlink: %+v", srcMap)
	}
	// Destination side: the linked entry is gone. Asserting by name
	// rather than index because tmux's auto-named first window may sit
	// at index 0 with a version-dependent label.
	dstMap := listWindowNames(t, extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "uwhd"})))
	for _, name := range dstMap {
		if name == "shared" {
			t.Errorf("dst still references the unlinked window: %+v", dstMap)
		}
	}
}

// TestHandle_UnlinkWindow_KillRemovesLastReference pins the kill=true
// branch end-to-end through the dispatcher. A window with only one
// reference (its source session) survives a kill=false unlink (tmux
// refuses) but is destroyed by a kill=true unlink. The boundary's job
// is to plumb the flag — we don't try to assert tmux's exact error
// phrasing for the no-kill refusal here because that's covered by the
// controller-level test.
func TestHandle_UnlinkWindow_KillRemovesLastReference(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "uwks", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "uwks"}}))
	})
	// Two extra windows so the session keeps holding together when we
	// destroy "ephemeral" — without these the unlink would also tear
	// down the session, blurring what the test is asserting.
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "uwks", "name": "keepalive", "command": "/bin/sh", "select": false,
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "uwks", "name": "ephemeral", "command": "/bin/sh", "select": false,
	})

	// kill=false: the unlink must be refused because the window has
	// only one reference. The dispatcher surfaces tmux's refusal as a
	// CodeInternal — we just assert the call errored out and the
	// window survived; the exact phrasing belongs to the controller
	// test.
	_, rerr := tools.Handle(ctx, "tools/call", mustJSON(t, map[string]any{
		"name": "unlink_window",
		"arguments": map[string]any{
			"target": "uwks:ephemeral",
		},
	}))
	if rerr == nil {
		t.Fatal("expected error when unlinking the only reference without kill=true")
	}

	// kill=true: the unlink proceeds and the underlying window is gone.
	got := extractText(t, callTool(t, tools, ctx, "unlink_window", map[string]any{
		"target": "uwks:ephemeral", "kill": true,
	}))
	if !strings.Contains(got, `"unlinked":true`) {
		t.Fatalf("unlink_window kill=true text = %q, want 'unlinked:true'", got)
	}
	srcMap := listWindowNames(t, extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "uwks"})))
	for _, name := range srcMap {
		if name == "ephemeral" {
			t.Errorf("ephemeral window survived a kill=true unlink: %+v", srcMap)
		}
	}
}

// TestHandle_UnlinkWindow_RejectsEmptyTarget locks the up-front empty-
// string guard so the dispatcher never builds a partial tmux target.
func TestHandle_UnlinkWindow_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "unlink_window",
		"arguments": map[string]any{"target": ""},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "target") {
		t.Errorf("error message %q should reference the target field", rerr.Message)
	}
}

// TestHandle_UnlinkWindow_RejectsBadTargets covers the regex check for
// each half of the target string — a stray quote / shell metachar or
// a missing colon must not slip through to the tmux argv.
func TestHandle_UnlinkWindow_RejectsBadTargets(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{
			"missing colon",
			map[string]any{"target": "demo"},
		},
		{
			"bad session half",
			map[string]any{"target": "bad name:0"},
		},
		{
			"bad window half",
			map[string]any{"target": "demo:0; rm -rf /"},
		},
		{
			"empty window half",
			map[string]any{"target": "demo:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "unlink_window", "arguments": tc.args,
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params for %s", tc.name)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_UnlinkWindow_RejectsUnknownField pins the schema's
// additionalProperties:false at runtime: a typo like "targets" must
// fail fast with -32602 instead of being silently dropped (which
// would let a partial target reach tmux). The handler enforces this
// via json.Decoder.DisallowUnknownFields.
func TestHandle_UnlinkWindow_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "unlink_window",
		"arguments": map[string]any{
			"target":  "demo:0",
			"targets": "typo",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for unknown field")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_UnlinkWindow_MissingSessionMapsCode pins the wire
// contract: unlink_window against an unknown session must surface
// CodeSessionNotFound (-32000), mirroring swap_window /
// window_select / window_move.
func TestHandle_UnlinkWindow_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so the dispatcher hits the "server up,
	// session missing" branch.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "uwanchor", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "uwanchor"}}))
	})

	params := mustJSON(t, map[string]any{
		"name": "unlink_window",
		"arguments": map[string]any{
			"target": "definitely_no_such_uw_session:0",
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

// TestHandle_ToolsList_IncludesUnlinkWindow makes sure the dispatch
// surface advertises the new tool so MCP clients can discover it via
// tools/list — including the strict additionalProperties contract every
// other window tool upholds. Pins the single required field too so a
// future contributor cannot silently drop it (which would weaken the
// schema's "fast-fail on a missing target" contract).
func TestHandle_ToolsList_IncludesUnlinkWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		name, _ := def["name"].(string)
		if name != "unlink_window" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		// additionalProperties:false is part of the contract — an agent
		// that misnames a field gets a fast schema-shaped rejection
		// rather than a silent no-op.
		if got, ok := schema["additionalProperties"].(bool); !ok || got {
			t.Errorf("schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		req, _ := schema["required"].([]string)
		if len(req) != 1 || req[0] != "target" {
			t.Errorf("required = %v, want [target]", req)
		}
		return
	}
	t.Fatalf("tools/list missing 'unlink_window'")
}

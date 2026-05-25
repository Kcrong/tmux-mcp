package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_DisplayPanes_HeadlessServerIsNoop pins the load-bearing
// path for the headless servers tmux-mcp owns: a display_panes call
// with no attached terminals must come back as a clean
// `{"displayed": true}` envelope rather than an error. Without that
// mapping every fire-and-forget display_panes would have to first run
// list_clients to know whether to skip.
func TestHandle_DisplayPanes_HeadlessServerIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the controller's tmux server is
	// definitely up — display-panes without a server returns a
	// different error shape we don't want to exercise here.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dp_noop", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "display_panes", map[string]any{}))
	var obj struct {
		Displayed bool `json:"displayed"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode display_panes: %v\nbody=%s", err, body)
	}
	if !obj.Displayed {
		t.Fatalf("expected displayed=true, got body=%s", body)
	}
}

// TestHandle_DisplayPanes_HeadlessWithTemplateIsNoop mirrors the
// noop path for the template-bearing variant. Forwarding a non-empty
// template must NOT change the headless-fold behaviour: tmux still
// reports "no current client" before it gets a chance to invoke the
// template, and the boundary must keep folding that onto a clean
// success.
func TestHandle_DisplayPanes_HeadlessWithTemplateIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dp_noop_tpl", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "display_panes", map[string]any{
		"template": "select-pane -t %%",
	}))
	var obj struct {
		Displayed bool `json:"displayed"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode display_panes: %v\nbody=%s", err, body)
	}
	if !obj.Displayed {
		t.Fatalf("expected displayed=true, got body=%s", body)
	}
}

// TestHandle_DisplayPanes_HappyPathSkipsWithoutAttachedClient covers
// the "real attached client" load-bearing path opportunistically.
// Spawning a tmux client requires a real PTY which is fragile inside
// CI; the conventional pattern (mirrored from detach_client tests) is
// to enumerate `list_clients` and skip when nothing is attached
// rather than fight the PTY. Keeping the test here pins the calling
// shape so a future PTY-enabled runner exercises the happy path
// without a code change.
func TestHandle_DisplayPanes_HappyPathSkipsWithoutAttachedClient(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dp_happy", "command": "/bin/sh",
	})

	listBody := extractText(t, callTool(t, tools, ctx, "list_clients", map[string]any{}))
	var lc struct {
		Clients []struct {
			TTY string `json:"tty"`
		} `json:"clients"`
	}
	if err := json.Unmarshal([]byte(listBody), &lc); err != nil {
		t.Fatalf("decode list_clients: %v\nbody=%s", err, listBody)
	}
	if len(lc.Clients) == 0 {
		t.Skip("no attached tmux clients on controller socket; can't exercise happy display_panes without a real PTY")
	}
	target := lc.Clients[0].TTY
	body := extractText(t, callTool(t, tools, ctx, "display_panes", map[string]any{
		"target":      target,
		"duration_ms": 50,
	}))
	var obj struct {
		Displayed bool `json:"displayed"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode display_panes: %v\nbody=%s", err, body)
	}
	if !obj.Displayed {
		t.Fatalf("expected displayed=true, got body=%s", body)
	}
}

// TestHandle_DisplayPanes_MissingClientMapsCode pins the wire contract
// that asking display_panes to draw on a non-existent client surfaces
// CodeSessionNotFound rather than a generic internal-error code,
// mirroring detach_client / list_clients / session_kill. The audit log
// relies on the typed code to record a stable failure category.
func TestHandle_DisplayPanes_MissingClientMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so the dispatcher hits
	// the "server up, named client does not exist" branch rather than
	// "no server running" (different stderr).
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dp_missing_anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name":      "display_panes",
		"arguments": map[string]any{"target": "/dev/pts/_definitely_does_not_exist_xyzzy"},
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

// TestHandle_DisplayPanes_RejectsBadTarget guards the regex/length
// policy on the optional `target` argument — even though it is
// optional, a present-but-malformed value must be refused with
// CodeInvalidParams up front so tmux is never asked to resolve it
// (defence against shell metachars / accidentally-quoted input).
func TestHandle_DisplayPanes_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []string{
		// Whitespace — never appears in a legitimate TTY path.
		"/dev/pts/3 with space",
		// Shell metachar — defence against accidental injection.
		"/dev/pts/3;rm -rf",
		// Backtick — same intent.
		"/dev/pts/3`whoami`",
		// Doesn't start with "/" — TTY paths are always absolute.
		"dev/pts/3",
		// Pipe metachar — never legitimate.
		"/dev/pts/3|cat",
	}
	for _, c := range cases {
		params := mustJSON(t, map[string]any{
			"name":      "display_panes",
			"arguments": map[string]any{"target": c},
		})
		_, rerr := tools.Handle(context.Background(), "tools/call", params)
		if rerr == nil {
			t.Errorf("expected invalid params error for target=%q", c)
			continue
		}
		if rerr.Code != errs.CodeInvalidParams {
			t.Errorf("target=%q: code = %d, want CodeInvalidParams (%d)",
				c, rerr.Code, errs.CodeInvalidParams)
		}
	}
}

// TestHandle_DisplayPanes_RejectsBadDuration guards the 10-minute cap
// on `duration_ms`. Negative values and values past the standard
// maxDurationMs (10 minutes, mirrored across every *_ms knob in the
// surface) must surface CodeInvalidParams up front so a hostile
// caller cannot pin a tmux client in the picker for unbounded
// durations.
func TestHandle_DisplayPanes_RejectsBadDuration(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []int{
		-1,
		-1000,
		maxDurationMs + 1,
		// Well past the cap: a typo with seconds-vs-ms confusion would
		// land here (e.g. "I'll just put 1e9 to keep it forever").
		1_000_000_000,
	}
	for _, c := range cases {
		params := mustJSON(t, map[string]any{
			"name":      "display_panes",
			"arguments": map[string]any{"duration_ms": c},
		})
		_, rerr := tools.Handle(context.Background(), "tools/call", params)
		if rerr == nil {
			t.Errorf("expected invalid params error for duration_ms=%d", c)
			continue
		}
		if rerr.Code != errs.CodeInvalidParams {
			t.Errorf("duration_ms=%d: code = %d, want CodeInvalidParams (%d)",
				c, rerr.Code, errs.CodeInvalidParams)
		}
	}
}

// TestHandle_DisplayPanes_RejectsOversizedTemplate pins the length cap
// on the optional `template` argument. The cap (4 KiB) is generous for
// any realistic tmux command but bounds the JSON-RPC payload so a
// hostile caller cannot inflate the argv with a megabyte of template
// body. A present-but-oversized template must surface CodeInvalidParams
// up front.
func TestHandle_DisplayPanes_RejectsOversizedTemplate(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	// Build a string strictly larger than the cap. strings.Repeat is
	// load-bearing here: the cap check is `len(tpl) > N`, and a
	// boundary value of exactly N must NOT trigger — only past-the-cap
	// rejects.
	tpl := strings.Repeat("a", displayPanesMaxTemplateLen+1)
	params := mustJSON(t, map[string]any{
		"name":      "display_panes",
		"arguments": map[string]any{"template": tpl},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized template")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "template length") {
		t.Fatalf("error message %q does not mention template length", rerr.Message)
	}
}

// TestHandle_DisplayPanes_RejectsUnknownField enforces the
// additionalProperties:false contract on the schema. A typo like
// "duration" (missing the _ms suffix) or "client" (instead of
// "target") must get a fast schema-shaped rejection rather than
// silently behaving like the unscoped variant. The handler uses a
// typed struct so extra fields are ignored at decode; we pin the
// contract through tools/list so spec-driven clients still see the
// locked schema surface.
func TestHandle_DisplayPanes_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "display_panes" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("display_panes schema additionalProperties = %v, want false",
				schema["additionalProperties"])
		}
		return
	}
	t.Fatalf("tools/list missing display_panes: %v", listing)
}

// TestHandle_ToolsList_IncludesDisplayPanes makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide the
// tool from the surface even though the dispatcher case still works
// for a hardcoded call.
func TestHandle_ToolsList_IncludesDisplayPanes(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "display_panes" {
			return
		}
	}
	t.Fatalf("tools/list missing display_panes")
}

// TestHandle_DisplayPanes_NotInReadOnlyAllowlist pins the policy
// classification: display_panes is a MUTATING tool (it draws onto a
// live client and can fire a templated tmux command on selection), so
// a -read-only deployment must NOT permit it. Mirrors the spec section
// that calls out the allowlist as the single source of truth — adding
// a tool here that turns out to be pure inspection is a one-line
// revert.
func TestHandle_DisplayPanes_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("display_panes") {
		t.Fatal("display_panes must not be in readOnlyTools — it draws onto live clients and can fire templated commands")
	}
}

// TestValidateDisplayPanesTarget_AcceptsRealisticTtyPaths keeps the
// regex honest against the shapes legitimate TTY paths actually take
// across platforms: Linux pseudo-tty, macOS pseudo-tty, USB serial
// adapters with dot-bearing names, and the rare ASCII-colon variant
// some terminal emulators advertise. Drift here would silently turn
// valid inputs into -32602 rejections for end-users who never typed
// anything malformed.
func TestValidateDisplayPanesTarget_AcceptsRealisticTtyPaths(t *testing.T) {
	t.Parallel()
	cases := []string{
		"/dev/pts/0",
		"/dev/pts/127",
		"/dev/ttys001",
		"/dev/tty.usbserial-1410",
		"/dev/pts/3:0",
		// Empty is allowed (the call resolves against the caller's
		// "current" client, folded onto a no-op on a headless server).
		"",
	}
	for _, c := range cases {
		if rerr := validateDisplayPanesTarget(c); rerr != nil {
			t.Errorf("validateDisplayPanesTarget(%q) = %v, want nil", c, rerr)
		}
	}
}

// TestHandle_DisplayPanes_BareCallIsAccepted pins the contract that a
// tools/call with no `arguments` object at all reaches the controller
// just like `arguments: {}` would: every flag defaults off, every
// string defaults empty, and on a headless server the result is a
// clean `{"displayed": true}` no-op. The handler handles len(raw)==0
// specifically; this test guards that behaviour.
func TestHandle_DisplayPanes_BareCallIsAccepted(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a session so the tmux server is up.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dp_bare", "command": "/bin/sh",
	})

	// Note the absence of `arguments` — the dispatcher hands the
	// handler a nil/empty payload here.
	params := mustJSON(t, map[string]any{"name": "display_panes"})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("display_panes bare call: %s (code=%d)", rerr.Message, rerr.Code)
	}
	body := extractText(t, res)
	var obj struct {
		Displayed bool `json:"displayed"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode display_panes: %v\nbody=%s", err, body)
	}
	if !obj.Displayed {
		t.Fatalf("expected displayed=true on bare call, got body=%s", body)
	}
}

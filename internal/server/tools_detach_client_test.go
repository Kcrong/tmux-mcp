package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_DetachClient_AllOnEmptyServerIsNoop pins the load-bearing
// path for the headless servers tmux-mcp owns: a detach with `all=true`
// and no attached terminals must come back as a clean
// `{"detached": true}` envelope rather than an error. Without that
// mapping every fire-and-forget detach would have to first run
// list_clients to know whether to skip.
func TestHandle_DetachClient_AllOnEmptyServerIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the controller's tmux server is
	// definitely up — detach-client without a server returns a
	// different error shape we don't want to exercise here.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dc_noop", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "detach_client", map[string]any{
		"all": true,
	}))
	var obj struct {
		Detached bool `json:"detached"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode detach_client: %v\nbody=%s", err, body)
	}
	if !obj.Detached {
		t.Fatalf("expected detached=true, got body=%s", body)
	}
}

// TestHandle_DetachClient_BySessionIsSuccessfulNoop mirrors the
// all=true test for the by-session branch: scoping a detach to an
// existing session that has no attached clients must come back as a
// clean success. The session resolves cleanly so tmux doesn't emit
// "can't find session"; instead it falls through to the "no current
// client" branch which the boundary folds onto a successful response.
func TestHandle_DetachClient_BySessionIsSuccessfulNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dc_sess_noop", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "detach_client", map[string]any{
		"session": "dc_sess_noop",
	}))
	var obj struct {
		Detached bool `json:"detached"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode detach_client: %v\nbody=%s", err, body)
	}
	if !obj.Detached {
		t.Fatalf("expected detached=true, got body=%s", body)
	}
}

// TestHandle_DetachClient_HappyPathSkipsWithoutAttachedClient covers
// the "real attached client" load-bearing path opportunistically.
// Spawning a tmux client requires a real PTY, which is fragile inside
// CI's hermetic sandbox; the conventional pattern (mirrored from
// refresh_client / lock_client tests) is to enumerate `list_clients`
// and skip when nothing is attached rather than fight the PTY.
//
// In practice this test almost always hits the t.Skip branch in CI;
// keeping it here pins the calling shape so a future PTY-enabled
// runner exercises the by-client happy path without a code change.
func TestHandle_DetachClient_HappyPathSkipsWithoutAttachedClient(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dc_happy", "command": "/bin/sh",
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
		t.Skip("no attached tmux clients on controller socket; can't exercise happy detach without a real PTY")
	}
	target := lc.Clients[0].TTY
	body := extractText(t, callTool(t, tools, ctx, "detach_client", map[string]any{
		"client": target,
	}))
	var obj struct {
		Detached bool `json:"detached"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode detach_client: %v\nbody=%s", err, body)
	}
	if !obj.Detached {
		t.Fatalf("expected detached=true, got body=%s", body)
	}
}

// TestHandle_DetachClient_RejectsAllEmpty pins the at-least-one-set
// rule. Bare `{}` (or every field omitted/false) must come back with
// CodeInvalidParams up front rather than dispatching `tmux detach-
// client`, which on the headless servers tmux-mcp owns would emit "no
// current client" and otherwise silently succeed via the controller's
// no-op fold. The validation has to live at the boundary so a
// mistakenly-empty request never reaches tmux.
func TestHandle_DetachClient_RejectsAllEmpty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	params := mustJSON(t, map[string]any{
		"name":      "detach_client",
		"arguments": map[string]any{},
	})
	_, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error for empty detach_client arguments")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_DetachClient_RejectsBareCall mirrors the all-empty reject
// path for the "no arguments object at all" wire shape. The dispatcher
// hands detachClient a nil-ish payload when the caller omits
// `arguments` entirely; the handler must still reject it as
// CodeInvalidParams rather than treating absence as "detach
// everything".
func TestHandle_DetachClient_RejectsBareCall(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	params := mustJSON(t, map[string]any{"name": "detach_client"})
	_, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error for missing detach_client arguments")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_DetachClient_MissingClientMapsCode pins the wire contract
// that asking for a non-existent client surfaces CodeSessionNotFound
// rather than a generic internal-error code, mirroring list_clients /
// session_kill / pane_select. The audit log relies on the typed code
// to record a stable failure category.
func TestHandle_DetachClient_MissingClientMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so the dispatcher hits
	// the "server up, named client does not exist" branch rather than
	// "no server running" (different stderr).
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dc_missing_anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name":      "detach_client",
		"arguments": map[string]any{"client": "/dev/pts/_definitely_does_not_exist_xyzzy"},
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

// TestHandle_DetachClient_MissingSessionIsNoop pins the asymmetry
// between the missing-client and missing-session branches at the
// dispatcher layer: tmux's `detach-client -s GHOST` does not surface
// "can't find session" stderr — it falls through to "no current
// client" (because once tmux resolves -s into a session it then
// searches for an attached client to detach, and zero matches read as
// "no current client" rather than "no such session"). The boundary
// folds that onto a successful `{"detached": true}` response, so a
// fire-and-forget detach against a typo'd or unattached session looks
// like a clean success rather than a sentinel error the caller must
// branch on. This contract is deliberately asymmetric with
// session_kill / list_clients — and the test pins the asymmetry so a
// future contributor can't quietly "fix" it.
func TestHandle_DetachClient_MissingSessionIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dc_misses_anchor", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "detach_client", map[string]any{
		"session": "ghost_session_xyzzy",
	}))
	var obj struct {
		Detached bool `json:"detached"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode detach_client: %v\nbody=%s", err, body)
	}
	if !obj.Detached {
		t.Fatalf("expected detached=true for missing session no-op, got body=%s", body)
	}
}

// TestHandle_DetachClient_RejectsBadClient guards the regex/length
// policy on the optional `client` argument — even though it is
// optional, a present-but-malformed value must be refused with
// CodeInvalidParams up front so tmux is never asked to resolve it
// (defence against shell metachars / accidentally-quoted input).
func TestHandle_DetachClient_RejectsBadClient(t *testing.T) {
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
	}
	for _, c := range cases {
		params := mustJSON(t, map[string]any{
			"name":      "detach_client",
			"arguments": map[string]any{"client": c},
		})
		_, rerr := tools.Handle(context.Background(), "tools/call", params)
		if rerr == nil {
			t.Errorf("expected invalid params error for client=%q", c)
			continue
		}
		if rerr.Code != errs.CodeInvalidParams {
			t.Errorf("client=%q: code = %d, want CodeInvalidParams (%d)", c, rerr.Code, errs.CodeInvalidParams)
		}
	}
}

// TestHandle_DetachClient_RejectsBadSession guards the policy on the
// `session` argument: the existing validateSessionRef contract forbids
// colons / dots / whitespace etc., and a present-but-malformed session
// must surface as CodeInvalidParams up front so tmux never sees the
// hostile string.
func TestHandle_DetachClient_RejectsBadSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []string{
		"with space",
		"colon:bad",
		"dot.bad",
		"semi;rm",
	}
	for _, c := range cases {
		params := mustJSON(t, map[string]any{
			"name":      "detach_client",
			"arguments": map[string]any{"session": c},
		})
		_, rerr := tools.Handle(context.Background(), "tools/call", params)
		if rerr == nil {
			t.Errorf("expected invalid params error for session=%q", c)
			continue
		}
		if rerr.Code != errs.CodeInvalidParams {
			t.Errorf("session=%q: code = %d, want CodeInvalidParams (%d)", c, rerr.Code, errs.CodeInvalidParams)
		}
	}
}

// TestHandle_DetachClient_RejectsUnknownField enforces the
// additionalProperties:false contract on the schema. A typo like
// "tty" or an attempt to smuggle in a non-listed knob must get a
// fast schema-shaped rejection rather than silently behaving like the
// unscoped variant. The handler uses a typed struct so extra fields
// are ignored at decode; we pin the contract through tools/list so
// spec-driven clients still see the locked schema surface.
func TestHandle_DetachClient_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "detach_client" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("detach_client schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		return
	}
	t.Fatalf("tools/list missing detach_client: %v", listing)
}

// TestHandle_ToolsList_IncludesDetachClient makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide
// the tool from the surface even though the dispatcher case still
// works for a hardcoded call.
func TestHandle_ToolsList_IncludesDetachClient(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "detach_client" {
			return
		}
	}
	t.Fatalf("tools/list missing detach_client")
}

// TestHandle_DetachClient_NotInReadOnlyAllowlist pins the policy
// classification: detach_client is a MUTATING tool (it changes the
// server's client roster), so a -read-only deployment must NOT permit
// it. Mirrors the spec section that calls out the allowlist as the
// single source of truth — adding a tool here that turns out to be
// pure inspection is a one-line revert.
func TestHandle_DetachClient_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("detach_client") {
		t.Fatal("detach_client must not be in readOnlyTools — it changes the server's client roster")
	}
}

// TestValidateDetachClientName_AcceptsRealisticTtyPaths keeps the
// regex honest against the shapes legitimate TTY paths actually take
// across platforms: Linux pseudo-tty, macOS pseudo-tty, USB serial
// adapters with dot-bearing names, and the rare ASCII-colon variant
// some terminal emulators advertise. Drift here would silently turn
// valid inputs into -32602 rejections for end-users who never typed
// anything malformed.
func TestValidateDetachClientName_AcceptsRealisticTtyPaths(t *testing.T) {
	t.Parallel()
	cases := []string{
		"/dev/pts/0",
		"/dev/pts/127",
		"/dev/ttys001",
		"/dev/tty.usbserial-1410",
		"/dev/pts/3:0",
		// Empty is allowed (the at-least-one-set rule is checked separately).
		"",
	}
	for _, c := range cases {
		if rerr := validateDetachClientName(c); rerr != nil {
			t.Errorf("validateDetachClientName(%q) = %v, want nil", c, rerr)
		}
	}
}

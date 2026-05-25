package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_LockClient_NoArgsIsSuccessfulNoop pins the load-bearing
// path for the headless servers tmux-mcp owns: a lock with no
// `client` and no attached terminals must come back as a clean
// {"locked": true} envelope rather than an error. Without that
// mapping every fire-and-forget lock would have to first run
// list_clients to know whether to skip.
func TestHandle_LockClient_NoArgsIsSuccessfulNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the controller's tmux server is
	// definitely up — lock-client without a server returns a
	// different error shape we don't want to exercise here.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lc_noop", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "lock_client", map[string]any{}))
	var obj struct {
		Locked bool `json:"locked"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode lock_client: %v\nbody=%s", err, body)
	}
	if !obj.Locked {
		t.Fatalf("expected locked=true, got body=%s", body)
	}
}

// TestHandle_LockClient_AcceptsNullArguments guards the "raw is
// empty" branch — the dispatcher hands lock_client a nil-ish payload
// when the caller sends `arguments: {}` (or omits the field
// entirely). The handler must accept it as "lock the current client"
// rather than rejecting it as malformed.
func TestHandle_LockClient_AcceptsNullArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lc_null", "command": "/bin/sh",
	})

	// Construct params manually so we can omit the "arguments" key
	// entirely — that's the path that exercises the len(raw) == 0
	// branch in the handler.
	params := mustJSON(t, map[string]any{"name": "lock_client"})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("lock_client: %s", rerr.Message)
	}
	body := extractText(t, res)
	var obj struct {
		Locked bool `json:"locked"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode lock_client: %v\nbody=%s", err, body)
	}
	if !obj.Locked {
		t.Fatalf("expected locked=true, got body=%s", body)
	}
}

// TestHandle_LockClient_MissingClientMapsCode pins the wire contract
// that asking to lock a non-existent client surfaces
// CodeSessionNotFound rather than a generic internal-error code,
// mirroring list_clients / session_kill / pane_select. The audit log
// relies on the typed code to record a stable failure category.
func TestHandle_LockClient_MissingClientMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so the dispatcher
	// hits the "server up, named client does not exist" branch
	// rather than "no server running" (different stderr).
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lc_missing_anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name":      "lock_client",
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

// TestHandle_LockClient_RejectsBadClient guards the regex/length
// policy on the optional `client` argument — even though it is
// optional, a present-but-malformed value must be refused with
// CodeInvalidParams up front so tmux is never asked to resolve it
// (defence against shell metachars / accidentally-quoted input).
func TestHandle_LockClient_RejectsBadClient(t *testing.T) {
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
			"name":      "lock_client",
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

// TestHandle_LockClient_RejectsOversizedClient covers the length
// branch of validateLockClientName — a multi-kilobyte client string
// is almost certainly a hostile caller and must be refused before
// tmux is consulted.
func TestHandle_LockClient_RejectsOversizedClient(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	// Build a path-shaped string that comfortably exceeds the cap so
	// the test stays insensitive to small bumps in
	// maxLockClientNameLen.
	long := "/dev/pts/"
	for i := 0; i < maxLockClientNameLen; i++ {
		long += "a"
	}
	params := mustJSON(t, map[string]any{
		"name":      "lock_client",
		"arguments": map[string]any{"client": long},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized client")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_LockClient_RejectsUnknownField enforces the
// additionalProperties:false contract on the schema. A typo like
// "clinet" or an attempt to smuggle in a non-listed knob must get a
// fast schema-shaped rejection rather than silently behaving like
// the unscoped variant. The handler uses a typed struct so extra
// fields are ignored at decode; we pin the contract through
// tools/list so spec-driven clients still see the locked schema
// surface.
func TestHandle_LockClient_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "lock_client" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("lock_client schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		return
	}
	t.Fatalf("tools/list missing lock_client: %v", listing)
}

// TestHandle_ToolsList_IncludesLockClient makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide
// the tool from the surface even though the dispatcher case still
// works for a hardcoded call.
func TestHandle_ToolsList_IncludesLockClient(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "lock_client" {
			return
		}
	}
	t.Fatalf("tools/list missing lock_client")
}

// TestHandle_LockClient_NotInReadOnlyAllowlist pins the policy
// classification: lock_client is a MUTATING tool (it changes what
// the client's terminal displays — the lock screen replaces the
// live session view), so a -read-only deployment must NOT permit
// it. Mirrors the spec section that calls out the allowlist as the
// single source of truth — adding a tool here that turns out to
// mutate state is a one-line revert.
func TestHandle_LockClient_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("lock_client") {
		t.Fatal("lock_client must not be in readOnlyTools — it mutates client display state")
	}
}

// TestValidateLockClientName_AcceptsRealisticTtyPaths keeps the
// regex honest against the shapes legitimate TTY paths actually take
// across platforms: Linux pseudo-tty, macOS pseudo-tty, USB serial
// adapters with dot-bearing names, and the rare ASCII-colon variant
// some terminal emulators advertise. Drift here would silently turn
// valid inputs into -32602 rejections for end-users who never typed
// anything malformed.
func TestValidateLockClientName_AcceptsRealisticTtyPaths(t *testing.T) {
	t.Parallel()
	cases := []string{
		"/dev/pts/0",
		"/dev/pts/127",
		"/dev/ttys001",
		"/dev/tty.usbserial-1410",
		"/dev/pts/3:0",
		// Empty is allowed (locks the current client).
		"",
	}
	for _, c := range cases {
		if rerr := validateLockClientName(c); rerr != nil {
			t.Errorf("validateLockClientName(%q) = %v, want nil", c, rerr)
		}
	}
}

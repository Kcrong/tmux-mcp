package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_ConfirmBefore_HeadlessMapsCode pins the load-bearing
// failure shape for the headless tmux servers tmux-mcp owns: with no
// client attached, asking tmux to display a y/n confirmation prompt
// must surface CodeSessionNotFound rather than a generic internal
// error or a silent success. The whole point of confirm_before is to
// avoid auto-executing destructive commands; a successful no-op
// would defeat that.
func TestHandle_ConfirmBefore_HeadlessMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the controller's tmux server is
	// definitely up — confirm-before without a server returns a
	// different stderr shape we want covered by the same sentinel
	// (so the caller branches on one code) but the load-bearing
	// path here is "server up, no client attached".
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cb_headless", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "confirm_before",
		"arguments": map[string]any{
			"prompt":  "go ahead?",
			"command": "display-message ok",
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

// TestHandle_ConfirmBefore_MissingClientMapsCode pins the wire
// contract that an unknown -t target surfaces CodeSessionNotFound
// rather than CodeInternal — mirroring list_clients / session_kill /
// lock_client so callers learn one stable code for "named target
// does not exist".
func TestHandle_ConfirmBefore_MissingClientMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cb_missing_anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "confirm_before",
		"arguments": map[string]any{
			"target":  "/dev/pts/_definitely_does_not_exist_xyzzy",
			"command": "display-message ok",
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

// TestHandle_ConfirmBefore_RejectsBadTarget guards the regex/length
// policy on the optional `target` argument — even though it is
// optional, a present-but-malformed value must be refused with
// CodeInvalidParams up front so tmux is never asked to resolve it
// (defence against shell metachars / accidentally-quoted input).
func TestHandle_ConfirmBefore_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	cases := []string{
		// Whitespace — never appears in a legitimate TTY path.
		"/dev/pts/3 with space",
		// Shell metachar — defence against accidental injection.
		"/dev/pts/3;rm -rf",
		// Backtick — same intent.
		"/dev/pts/3`whoami`",
		// Glob — never legitimate in a -t target.
		"/dev/pts/*",
	}
	for _, c := range cases {
		params := mustJSON(t, map[string]any{
			"name": "confirm_before",
			"arguments": map[string]any{
				"target":  c,
				"command": "display-message ok",
			},
		})
		_, rerr := tools.Handle(context.Background(), "tools/call", params)
		if rerr == nil {
			t.Errorf("expected invalid params error for target=%q", c)
			continue
		}
		if rerr.Code != errs.CodeInvalidParams {
			t.Errorf("target=%q: code = %d, want CodeInvalidParams (%d)", c, rerr.Code, errs.CodeInvalidParams)
		}
	}
}

// TestHandle_ConfirmBefore_RejectsMissingCommand pins the contract
// that `command` is REQUIRED. Without this guard a caller who forgot
// the field would discover the omission only when tmux's usage
// stderr made it back through the generic CodeInternal path; the
// explicit -32602 here is a much clearer schema-shaped failure.
func TestHandle_ConfirmBefore_RejectsMissingCommand(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "confirm_before",
		"arguments": map[string]any{
			"prompt": "go ahead?",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ConfirmBefore_RejectsUnknownField enforces the
// additionalProperties:false contract on the schema. A typo like
// "cmd" or "client" must get a fast schema-shaped surface (a tools/
// list inspection of the locked schema) so spec-driven clients see
// the policy without ambiguity.
func TestHandle_ConfirmBefore_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "confirm_before" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("confirm_before schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		// Required field pin — the schema must declare command as
		// the single required field so spec-driven clients enforce
		// it before the request even leaves the wire.
		req, _ := schema["required"].([]string)
		if len(req) != 1 || req[0] != "command" {
			t.Fatalf("confirm_before schema required = %v, want [command]", req)
		}
		return
	}
	t.Fatalf("tools/list missing confirm_before: %v", listing)
}

// TestHandle_ToolsList_IncludesConfirmBefore makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide
// the tool from the surface even though the dispatcher case still
// works for a hardcoded call.
func TestHandle_ToolsList_IncludesConfirmBefore(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "confirm_before" {
			return
		}
	}
	t.Fatalf("tools/list missing confirm_before")
}

// TestHandle_ConfirmBefore_NotInReadOnlyAllowlist pins the policy
// classification: confirm_before is a MUTATING tool (its whole
// purpose is to queue a destructive command behind a y/n prompt;
// when the user accepts, tmux runs whatever was passed in
// `command`), so a -read-only deployment must NOT permit it.
// Mirrors the spec section that calls out the allowlist as the
// single source of truth — adding a tool here that turns out to
// mutate state is a one-line revert.
func TestHandle_ConfirmBefore_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("confirm_before") {
		t.Fatal("confirm_before must not be in readOnlyTools — it stages a destructive command")
	}
}

// TestValidateConfirmTarget_AcceptsRealisticTtyPaths keeps the regex
// honest against the shapes legitimate tmux client targets actually
// take across platforms: Linux pseudo-tty, macOS pseudo-tty, USB
// serial adapters with dot-bearing names, and the rare alias-shaped
// target some operators use. Drift here would silently turn valid
// inputs into -32602 rejections for end-users who never typed
// anything malformed.
func TestValidateConfirmTarget_AcceptsRealisticTtyPaths(t *testing.T) {
	t.Parallel()
	cases := []string{
		"/dev/pts/0",
		"/dev/pts/127",
		"/dev/ttys001",
		"/dev/tty.usbserial-1410",
		"alias_client.0",
		// Empty is allowed (uses the caller's current client).
		"",
	}
	for _, c := range cases {
		if rerr := validateConfirmTarget(c); rerr != nil {
			t.Errorf("validateConfirmTarget(%q) = %v, want nil", c, rerr)
		}
	}
}

// TestHandle_ConfirmBefore_RejectsOversizedCommand covers the length
// branch on the REQUIRED command argument. A multi-kilobyte command
// is almost certainly a hostile caller and must be refused before
// tmux is consulted (tmux happily accepts long commands but a 4 KiB
// ceiling is generous enough for chained destructive commands while
// keeping the JSON-RPC frame size predictable).
func TestHandle_ConfirmBefore_RejectsOversizedCommand(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	long := "display-message " + strings.Repeat("a", maxConfirmCommandLen)
	params := mustJSON(t, map[string]any{
		"name": "confirm_before",
		"arguments": map[string]any{
			"command": long,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

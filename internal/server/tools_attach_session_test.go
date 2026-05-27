package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_AttachSession_HeadlessDetachOthers drives the load-bearing
// happy path: with the target session present and DetachOthers=true,
// the handler must succeed as a clean no-op and echo back the logical
// session name. This is the canonical "use attach_session to clear the
// client roster from a headless MCP server" shape.
func TestHandle_AttachSession_HeadlessDetachOthers(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "as_dh", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "attach_session", map[string]any{
		"target_session": "as_dh",
		"detach_others":  true,
	}))
	var obj struct {
		Attached      bool   `json:"attached"`
		TargetSession string `json:"target_session"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode attach_session: %v\nbody=%s", err, body)
	}
	if !obj.Attached {
		t.Fatalf("attached = false, want true; body=%s", body)
	}
	if obj.TargetSession != "as_dh" {
		t.Fatalf("target_session = %q, want \"as_dh\"; body=%s", obj.TargetSession, body)
	}
}

// TestHandle_AttachSession_NoDetachReturnsInvalidParams pins the
// load-bearing TTY-refusal path: a request without any detach flag set
// must come back with CodeInvalidParams and a message that mentions
// the real-terminal escape hatch. Without this pin a future refactor
// could regress the user-facing diagnostic and the test suite would
// silently let it through.
func TestHandle_AttachSession_NoDetachReturnsInvalidParams(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "as_notty", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "attach_session",
		"arguments": map[string]any{
			"target_session": "as_notty",
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for no-detach attach, got result %#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
	// The message must direct the user at the real-terminal escape
	// hatch — substring-match on "tmux attach -t" is the load-bearing
	// guarantee, not the exact phrasing.
	if !strings.Contains(rerr.Message, "tmux attach -t") {
		t.Fatalf("error message %q does not direct user to `tmux attach -t`", rerr.Message)
	}
	if !strings.Contains(rerr.Message, "as_notty") {
		t.Fatalf("error message %q does not mention the target session name", rerr.Message)
	}
}

// TestHandle_AttachSession_MissingSessionMapsCode pins the wire
// contract that asking for a non-existent session surfaces
// CodeSessionNotFound rather than a generic internal-error code,
// mirroring every other targeted tool. The audit log relies on the
// typed code to record a stable failure category.
func TestHandle_AttachSession_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so the dispatcher
	// hits the "server is up but the named session does not exist"
	// branch rather than "no server running" (different stderr).
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "as_anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "attach_session",
		"arguments": map[string]any{
			"target_session": "ghost_session_xyz",
			"detach_others":  true,
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for missing session, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_AttachSession_RejectsBadTarget guards the regex/length
// policy on the required `target_session` argument — a malformed value
// must be refused with CodeInvalidParams up front so tmux is never
// asked to resolve it (defence against shell metachars / accidentally-
// quoted input).
func TestHandle_AttachSession_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	for _, tc := range []struct {
		name   string
		target string
	}{
		{"empty", ""},
		{"with_space", "bad name"},
		{"with_colon", "name:0"},
		{"with_quote", "name\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "attach_session",
				"arguments": map[string]any{
					"target_session": tc.target,
					"detach_others":  true,
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for target_session %q", tc.target)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d) for target_session %q",
					rerr.Code, errs.CodeInvalidParams, tc.target)
			}
		})
	}
}

// TestHandle_AttachSession_RejectsBadFlags pins the validation on the
// optional `flags` argument: a value containing whitespace, shell
// metachars, or otherwise stray bytes must be refused so a future tmux
// version cannot be tricked into honouring it as multiple argv tokens.
func TestHandle_AttachSession_RejectsBadFlags(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	for _, tc := range []struct {
		name  string
		flags string
	}{
		{"with_space", "active-pane read-only"},
		{"with_quote", `active-pane"`},
		{"with_semicolon", "active-pane;rm -rf /"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "attach_session",
				"arguments": map[string]any{
					"target_session": "demo",
					"detach_others":  true,
					"flags":          tc.flags,
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for flags %q", tc.flags)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d) for flags %q",
					rerr.Code, errs.CodeInvalidParams, tc.flags)
			}
		})
	}
}

// TestHandle_AttachSession_RejectsRelativeWorkingDirectory pins the
// absolute-path policy on `working_directory`: a relative value must be
// refused up front so tmux is never asked to resolve it against the
// MCP server's own cwd (rarely what the caller intended).
func TestHandle_AttachSession_RejectsRelativeWorkingDirectory(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name": "attach_session",
		"arguments": map[string]any{
			"target_session":    "demo",
			"detach_others":     true,
			"working_directory": "relative/path",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for relative working_directory")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)",
			rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_AttachSession_AcceptsForwardCompatFlags exercises the
// forward-compat fields under the headless detach path: passing them
// alongside detach_others=true must still succeed. The fields are
// inert today but accepted for shape so MCP clients can populate them
// without seeing a future-version-only error today.
func TestHandle_AttachSession_AcceptsForwardCompatFlags(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "as_fwd", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "attach_session", map[string]any{
		"target_session":               "as_fwd",
		"detach_others_including_self": true,
		"read_only":                    true,
		"working_directory":            "/tmp",
		"skip_environment_update":      true,
		"flags":                        "active-pane,read-only",
		"no_environment_apply":         true,
	}))
	var obj struct {
		Attached bool `json:"attached"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode attach_session: %v\nbody=%s", err, body)
	}
	if !obj.Attached {
		t.Fatalf("attached = false, want true; body=%s", body)
	}
}

// TestHandle_AttachSession_RejectsUnknownField enforces the
// additionalProperties:false contract on the schema: a typo'd field
// (e.g. "session" instead of "target_session") must surface as a fast
// schema-shaped rejection rather than silently being ignored. The
// handler uses a typed struct so extra fields are dropped at decode,
// so we instead assert that the schema entry locks the surface for
// spec-driven clients.
func TestHandle_AttachSession_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "attach_session" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("attach_session schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		required, _ := schema["required"].([]string)
		var sawTarget bool
		for _, r := range required {
			if r == "target_session" {
				sawTarget = true
			}
		}
		if !sawTarget {
			t.Fatalf("attach_session schema required = %v, must contain target_session", required)
		}
		return
	}
	t.Fatalf("tools/list missing attach_session: %v", listing)
}

// TestHandle_ToolsList_IncludesAttachSession makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. A regression in init() registration would
// otherwise hide the tool from the surface even though the dispatcher
// case still works for a hardcoded call.
func TestHandle_ToolsList_IncludesAttachSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "attach_session" {
			return
		}
	}
	t.Fatalf("tools/list missing attach_session")
}

// TestIsReadOnlyTool_RejectsAttachSession is the load-bearing inverse
// of the inspection allowlist: attach_session is a MUTATOR (it can
// detach existing clients), so it must never be on the read-only
// allowlist. A future refactor that accidentally added it to
// readOnlyTools would let a -read-only deployment quietly clear the
// client roster of any session — pinning the rejection here keeps the
// policy uniform with every other mutating tool.
func TestIsReadOnlyTool_RejectsAttachSession(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("attach_session") {
		t.Fatal("IsReadOnlyTool(\"attach_session\") = true, want false (attach_session is a mutating tool)")
	}
}

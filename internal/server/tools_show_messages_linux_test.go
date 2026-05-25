//go:build linux

package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// Linux-only because macOS tmux records its own client commands
// (new-session, show-messages, …) into the per-client message log even
// on a headless server, so the "headless = empty messages" contract
// these tests pin only holds on Linux. The wire contracts they guard
// (empty-list shape; CodeSessionNotFound mapping) still exercise the
// same handler code paths regardless of platform — the macOS-only
// counterpart would need to seed messages and assert behavior with a
// real attached client, which is brittle in CI.

// TestHandle_ShowMessages_HeadlessReturnsEmptyList drives the
// load-bearing "no current client" path through the dispatcher. The
// headless tmux servers tmux-mcp owns rarely have a client attached;
// `tmux show-messages` reports "no current client" with rc=1 in that
// case, and the tool must surface a clean empty list rather than an
// error so an agent can introspect at any time without first having
// to attach a client.
func TestHandle_ShowMessages_HeadlessReturnsEmptyList(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the controller's tmux server is
	// definitely up. With nothing attached, show-messages still
	// reports "no current client" — but the boundary returns the
	// empty-list contract.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sm_anchor", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "show_messages", map[string]any{}))
	var obj struct {
		Messages []string `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode show_messages: %v\nbody=%s", err, body)
	}
	if obj.Messages == nil {
		t.Fatalf("expected non-nil messages slice (the wire shape must always be a list, never null); body=%s", body)
	}
	if len(obj.Messages) != 0 {
		t.Fatalf("expected zero messages on a headless server, got %d (%s)", len(obj.Messages), body)
	}
}

// TestHandle_ShowMessages_MissingClientMapsCode pins the wire contract
// that asking for a non-existent client surfaces CodeSessionNotFound
// rather than a generic internal-error code, mirroring every other
// targeted inspection tool. The audit log relies on the typed code
// to record a stable failure category.
func TestHandle_ShowMessages_MissingClientMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so the dispatcher
	// hits the "server is up but the named client does not exist"
	// branch rather than "no server running" (different stderr).
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sm_missing_anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name":      "show_messages",
		"arguments": map[string]any{"client": "/dev/pts/ghost_does_not_exist"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for missing client, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

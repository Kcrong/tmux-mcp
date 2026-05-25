package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestValidate_ToolCall_Failures exercises every tool's validation path
// without touching tmux. The handlers should return -32602 (invalid
// params) before any tmux command is run, so a bare *Tools (with no
// real Controller) is fine.
func TestValidate_ToolCall_Failures(t *testing.T) {
	t.Parallel()
	// A nil controller suffices because none of these inputs should
	// ever reach the tmux call — they must fail validation first.
	tools := &Tools{}

	cases := []struct {
		name     string
		tool     string
		args     any
		wantSubs []string // every substring must appear in the error message
	}{
		// session_create
		{
			name:     "session_create empty name",
			tool:     "session_create",
			args:     map[string]any{"name": "", "width": 80, "height": 24},
			wantSubs: []string{"session name required"},
		},
		{
			name:     "session_create bad chars",
			tool:     "session_create",
			args:     map[string]any{"name": "demo:colon", "width": 80, "height": 24},
			wantSubs: []string{"demo:colon", "must match"},
		},
		{
			name: "session_create name too long",
			tool: "session_create",
			args: map[string]any{
				"name":   strings.Repeat("a", 65),
				"width":  80,
				"height": 24,
			},
			wantSubs: []string{"session name length", "65"},
		},
		{
			name:     "session_create width too large",
			tool:     "session_create",
			args:     map[string]any{"name": "ok", "width": 999999, "height": 24},
			wantSubs: []string{"width 999999", "out of range"},
		},
		{
			name:     "session_create width too small",
			tool:     "session_create",
			args:     map[string]any{"name": "ok", "width": 5, "height": 24},
			wantSubs: []string{"width 5", "out of range"},
		},
		{
			name:     "session_create height too large",
			tool:     "session_create",
			args:     map[string]any{"name": "ok", "width": 80, "height": 9999},
			wantSubs: []string{"height 9999", "out of range"},
		},
		{
			name: "session_create relative cwd",
			tool: "session_create",
			args: map[string]any{
				"name":   "ok",
				"width":  80,
				"height": 24,
				"cwd":    "relative/path",
			},
			wantSubs: []string{"cwd", "relative/path", "absolute"},
		},

		// session_kill
		{
			name:     "session_kill empty",
			tool:     "session_kill",
			args:     map[string]any{"name": ""},
			wantSubs: []string{"session name required"},
		},
		{
			name:     "session_kill bad chars",
			tool:     "session_kill",
			args:     map[string]any{"name": "../sneaky"},
			wantSubs: []string{"must match"},
		},

		// send_keys
		{
			name:     "send_keys empty session",
			tool:     "send_keys",
			args:     map[string]any{"session": "", "keys": []string{"x"}},
			wantSubs: []string{"session required"},
		},
		{
			name:     "send_keys empty keys",
			tool:     "send_keys",
			args:     map[string]any{"session": "demo", "keys": []string{}},
			wantSubs: []string{"keys array", "non-empty"},
		},

		// capture
		{
			name:     "capture empty session",
			tool:     "capture",
			args:     map[string]any{"session": ""},
			wantSubs: []string{"session required"},
		},
		{
			name:     "capture invalid mode",
			tool:     "capture",
			args:     map[string]any{"session": "demo", "mode": "all"},
			wantSubs: []string{"capture mode", "all", "visible", "scrollback"},
		},

		// wait_for_stable
		{
			name:     "wait_for_stable empty session",
			tool:     "wait_for_stable",
			args:     map[string]any{"session": ""},
			wantSubs: []string{"session required"},
		},
		{
			name: "wait_for_stable timeout too large",
			tool: "wait_for_stable",
			args: map[string]any{
				"session":    "demo",
				"timeout_ms": 600001,
			},
			wantSubs: []string{"timeout_ms 600001", "out of range"},
		},
		{
			name: "wait_for_stable negative quiet_ms",
			tool: "wait_for_stable",
			args: map[string]any{
				"session":  "demo",
				"quiet_ms": -1,
			},
			wantSubs: []string{"quiet_ms -1", "out of range"},
		},

		// wait_for_text
		{
			name:     "wait_for_text empty session",
			tool:     "wait_for_text",
			args:     map[string]any{"session": "", "pattern": ".*"},
			wantSubs: []string{"session required"},
		},
		{
			name: "wait_for_text timeout too large",
			tool: "wait_for_text",
			args: map[string]any{
				"session":    "demo",
				"pattern":    ".*",
				"timeout_ms": 700000,
			},
			wantSubs: []string{"timeout_ms 700000", "out of range"},
		},

		// snapshot_diff
		{
			name:     "snapshot_diff empty session",
			tool:     "snapshot_diff",
			args:     map[string]any{"session": ""},
			wantSubs: []string{"session required"},
		},
		{
			name:     "snapshot_diff bad session chars",
			tool:     "snapshot_diff",
			args:     map[string]any{"session": "bad name"},
			wantSubs: []string{"must match"},
		},

		// resize
		{
			name:     "resize empty session",
			tool:     "resize",
			args:     map[string]any{"session": "", "width": 80, "height": 24},
			wantSubs: []string{"session required"},
		},
		{
			name:     "resize width too large",
			tool:     "resize",
			args:     map[string]any{"session": "demo", "width": 999999, "height": 24},
			wantSubs: []string{"width 999999", "out of range"},
		},
		{
			name:     "resize height too small",
			tool:     "resize",
			args:     map[string]any{"session": "demo", "width": 80, "height": 1},
			wantSubs: []string{"height 1", "out of range"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			argsRaw, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}
			params, err := json.Marshal(map[string]any{
				"name":      tc.tool,
				"arguments": json.RawMessage(argsRaw),
			})
			if err != nil {
				t.Fatalf("marshal params: %v", err)
			}
			res, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected error, got result %#v", res)
			}
			if rerr.Code != codeInvalidParams {
				t.Fatalf("expected code %d (invalid params), got %d (message=%q)",
					codeInvalidParams, rerr.Code, rerr.Message)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(rerr.Message, sub) {
					t.Errorf("error message %q missing substring %q", rerr.Message, sub)
				}
			}
		})
	}
}

// TestValidate_AcceptsLiberalDefaults guards against over-tightening:
// realistic inputs (large but plausible terminal, 5-minute timeout)
// must still pass validation. The actual tmux call is allowed to fail
// because we use a nil controller — we only assert that validation
// itself does not reject the input.
func TestValidate_AcceptsLiberalDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tool string
		args any
	}{
		{
			name: "session_create big-but-plausible terminal",
			tool: "session_create",
			args: map[string]any{
				"name": "ok", "width": 500, "height": 200,
				"cwd": "/tmp",
			},
		},
		{
			name: "session_create defaults (zero width/height)",
			tool: "session_create",
			args: map[string]any{"name": "ok"},
		},
		{
			name: "wait_for_stable five minute timeout",
			tool: "wait_for_stable",
			args: map[string]any{
				"session": "demo", "timeout_ms": 300000, "quiet_ms": 500,
			},
		},
		{
			name: "wait_for_text ten minute timeout",
			tool: "wait_for_text",
			args: map[string]any{
				"session": "demo", "pattern": ".*", "timeout_ms": 600000,
			},
		},
		{
			name: "send_keys single key",
			tool: "send_keys",
			args: map[string]any{
				"session": "demo", "keys": []string{"echo hi"},
			},
		},
		{
			name: "capture default mode",
			tool: "capture",
			args: map[string]any{"session": "demo"},
		},
		{
			name: "capture explicit visible",
			tool: "capture",
			args: map[string]any{"session": "demo", "mode": "visible"},
		},
		{
			name: "capture scrollback",
			tool: "capture",
			args: map[string]any{"session": "demo", "mode": "scrollback"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			argsRaw, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}
			params, err := json.Marshal(map[string]any{
				"name":      tc.tool,
				"arguments": json.RawMessage(argsRaw),
			})
			if err != nil {
				t.Fatalf("marshal params: %v", err)
			}
			tools := &Tools{}
			// Validation must not reject these. Anything that does reach
			// the (nil) controller will panic; we recover and treat that
			// as "validation passed" — we only fail when the *validation
			// layer* itself returned -32602.
			defer func() {
				_ = recover()
			}()
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr != nil && rerr.Code == codeInvalidParams {
				t.Fatalf("liberal input rejected by validator: %s", rerr.Message)
			}
		})
	}
}

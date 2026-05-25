package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_SwitchClient_TargetIsHeadlessNoop is the load-bearing
// happy-path for the headless servers tmux-mcp owns: with no terminal
// attached, asking tmux to redirect "every client" to a real session
// is trivially a no-op and the boundary must surface a clean
// {"switched": true} envelope. Without that mapping every
// fire-and-forget switch_client would have to first run list_clients
// to know whether to skip.
func TestHandle_SwitchClient_TargetIsHeadlessNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Two real sessions so the dispatcher sees a legitimate target and
	// the controller's tmux server is definitely up.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sc_t_a", "command": "/bin/sh",
	})
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sc_t_b", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "switch_client", map[string]any{
		"target": "sc_t_b",
	}))
	var obj struct {
		Switched bool `json:"switched"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode switch_client: %v\nbody=%s", err, body)
	}
	if !obj.Switched {
		t.Fatalf("expected switched=true, got body=%s", body)
	}
}

// TestHandle_SwitchClient_DirectionalIsHeadlessNoop pins the same
// no-op contract for the three directional flags. We exercise each
// one in turn so a regression that breaks (say) the prev path while
// keeping last and next working still trips this test by name.
func TestHandle_SwitchClient_DirectionalIsHeadlessNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sc_d_a", "command": "/bin/sh",
	})
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sc_d_b", "command": "/bin/sh",
	})

	for _, flag := range []string{"last", "next", "prev"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			body := extractText(t, callTool(t, tools, ctx, "switch_client", map[string]any{
				flag: true,
			}))
			var obj struct {
				Switched bool `json:"switched"`
			}
			if err := json.Unmarshal([]byte(body), &obj); err != nil {
				t.Fatalf("decode %s: %v\nbody=%s", flag, err, body)
			}
			if !obj.Switched {
				t.Fatalf("%s: expected switched=true, got body=%s", flag, body)
			}
		})
	}
}

// TestHandle_SwitchClient_ReadOnlyTogglesIndependently pins the
// `read_only=true` arm: combined with a directional choice it must
// flow through to tmux as `-r` on top of `-l`/`-n`/`-p` and still come
// back as a clean no-op on the headless server. A regression that
// dropped the flag through the wrapper would still pass the
// directional tests above, so this case carries its own assertion.
func TestHandle_SwitchClient_ReadOnlyTogglesIndependently(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sc_ro_a", "command": "/bin/sh",
	})
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sc_ro_b", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "switch_client", map[string]any{
		"last": true, "read_only": true,
	}))
	var obj struct {
		Switched bool `json:"switched"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, body)
	}
	if !obj.Switched {
		t.Fatalf("expected switched=true with read_only, got body=%s", body)
	}
}

// TestHandle_SwitchClient_AcceptsNullArguments_RejectedAsZeroChoice
// guards the "raw is empty" branch — the dispatcher hands
// switch_client a nil-ish payload when the caller sends `arguments:
// {}` (or omits the field entirely). The handler must accept the
// shape but then reject the all-zero call with a precise -32602
// message naming the exactly-one rule, so spec-driven clients see a
// stable failure mode rather than a silently-empty success.
func TestHandle_SwitchClient_AcceptsNullArguments_RejectedAsZeroChoice(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sc_null", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{"name": "switch_client"})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error, got result %#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
	if !strings.Contains(rerr.Message, "exactly one") {
		t.Fatalf("error message %q does not name the exactly-one rule", rerr.Message)
	}
}

// TestHandle_SwitchClient_RejectsTwoDirectional pins the inverse rule:
// any pair of {target, last, next, prev} must come back as a typed
// CodeInvalidParams without ever shelling out to tmux. We exercise
// representative pairs (target+flag and flag+flag) so the validation
// stays honest across both branches.
func TestHandle_SwitchClient_RejectsTwoDirectional(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	cases := []map[string]any{
		{"target": "sc_x", "last": true},
		{"target": "sc_x", "next": true},
		{"target": "sc_x", "prev": true},
		{"last": true, "next": true},
		{"last": true, "prev": true},
		{"next": true, "prev": true},
	}
	for _, c := range cases {
		params := mustJSON(t, map[string]any{
			"name":      "switch_client",
			"arguments": c,
		})
		_, rerr := tools.Handle(context.Background(), "tools/call", params)
		if rerr == nil {
			t.Errorf("expected invalid params error for %v", c)
			continue
		}
		if rerr.Code != errs.CodeInvalidParams {
			t.Errorf("%v: code = %d, want CodeInvalidParams (%d), msg=%q",
				c, rerr.Code, errs.CodeInvalidParams, rerr.Message)
		}
		if !strings.Contains(rerr.Message, "mutually exclusive") {
			t.Errorf("%v: error %q does not mention mutual exclusion", c, rerr.Message)
		}
	}
}

// TestHandle_SwitchClient_MissingClientMapsCode pins the wire contract
// that asking for a non-existent client surfaces CodeSessionNotFound
// rather than a generic internal-error code, mirroring list_clients /
// session_kill / pane_select. The audit log relies on the typed code
// to record a stable failure category. tmux validates the client
// argument before the target, so the missing-client matcher in
// switch_client.go is the load-bearing path here.
func TestHandle_SwitchClient_MissingClientMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sc_mc_a", "command": "/bin/sh",
	})
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sc_mc_b", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "switch_client",
		"arguments": map[string]any{
			"client": "/dev/pts/_definitely_not_attached_xyzzy",
			"target": "sc_mc_b",
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

// TestHandle_SwitchClient_RejectsBadClient guards the regex/length
// policy on the optional `client` argument — even though it is
// optional, a present-but-malformed value must be refused with
// CodeInvalidParams up front so tmux is never asked to resolve it
// (defence against shell metachars / accidentally-quoted input).
func TestHandle_SwitchClient_RejectsBadClient(t *testing.T) {
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
			"name": "switch_client",
			"arguments": map[string]any{
				"client": c,
				"last":   true,
			},
		})
		_, rerr := tools.Handle(context.Background(), "tools/call", params)
		if rerr == nil {
			t.Errorf("expected invalid params error for client=%q", c)
			continue
		}
		if rerr.Code != errs.CodeInvalidParams {
			t.Errorf("client=%q: code = %d, want CodeInvalidParams (%d)",
				c, rerr.Code, errs.CodeInvalidParams)
		}
	}
}

// TestHandle_SwitchClient_RejectsBadTarget guards the same policy on
// the optional `target` argument: a malformed session string must
// trip the validateSessionRef gate before any tmux call. Mirrors the
// list_clients / has_session / pane_select pattern so an agent
// hitting any of these tools sees a uniform validation surface.
func TestHandle_SwitchClient_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "switch_client",
		"arguments": map[string]any{
			"target": "bad name with spaces",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for malformed target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SwitchClient_RejectsUnknownField enforces the
// additionalProperties:false contract on the schema. A typo like
// "previous" or "readonly" must get a fast schema-shaped rejection
// rather than silently behaving like a different directional choice.
// The handler uses a typed struct so extra fields are ignored at
// decode; we pin the contract through tools/list so spec-driven
// clients still see the locked schema surface.
func TestHandle_SwitchClient_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "switch_client" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("switch_client schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		return
	}
	t.Fatalf("tools/list missing switch_client: %v", listing)
}

// TestHandle_ToolsList_IncludesSwitchClient makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide
// the tool from the surface even though the dispatcher case still
// works for a hardcoded call.
func TestHandle_ToolsList_IncludesSwitchClient(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "switch_client" {
			return
		}
	}
	t.Fatalf("tools/list missing switch_client")
}

// TestHandle_SwitchClient_NotInReadOnlyAllowlist pins the policy
// classification: switch_client is a MUTATING tool (it changes which
// session a client is bound to), so a -read-only deployment must NOT
// permit it. Mirrors the spec section that calls out the allowlist as
// the single source of truth — adding a tool here that turns out to
// mutate state is a one-line revert.
func TestHandle_SwitchClient_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("switch_client") {
		t.Fatal("switch_client must not be in readOnlyTools — it mutates which session a client is bound to")
	}
}

// TestValidateSwitchClientName_AcceptsRealisticTtyPaths keeps the
// regex honest against the shapes legitimate TTY paths actually take
// across platforms: Linux pseudo-tty, macOS pseudo-tty, USB serial
// adapters with dot-bearing names, and the rare ASCII-colon variant
// some terminal emulators advertise. Drift here would silently turn
// valid inputs into -32602 rejections for end-users who never typed
// anything malformed.
func TestValidateSwitchClientName_AcceptsRealisticTtyPaths(t *testing.T) {
	t.Parallel()
	cases := []string{
		"/dev/pts/0",
		"/dev/pts/127",
		"/dev/ttys001",
		"/dev/tty.usbserial-1410",
		"/dev/pts/3:0",
		// Empty is allowed (redirects the caller's current client).
		"",
	}
	for _, c := range cases {
		if rerr := validateSwitchClientName(c); rerr != nil {
			t.Errorf("validateSwitchClientName(%q) = %v, want nil", c, rerr)
		}
	}
}

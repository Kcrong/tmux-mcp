package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_DisplayPopup_RejectsBadTarget pins the up-front guard:
// a target that does not match paneTargetRE must trip CodeInvalidParams
// before any tmux invocation runs. Without this guard a stray quote
// or shell metachar would slip into argv where the tmux diagnostic is
// version-dependent and far from the operator's mistake.
func TestHandle_DisplayPopup_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name": "display_popup",
		"arguments": map[string]any{
			"target": "bad target with spaces",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestHandle_DisplayPopup_RejectsOversizedTitle pins the 4 KiB ceiling
// applied to the free-form `title` argument. tmux's format DSL is
// recursive and accepts very long inputs, but a realistic popup title
// is a few words; bounding the payload here keeps the JSON-RPC frame
// size predictable and refuses runaway / hostile callers up front.
func TestHandle_DisplayPopup_RejectsOversizedTitle(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	long := strings.Repeat("a", maxDisplayPopupFreeFormLen+1)
	params := mustJSON(t, map[string]any{
		"name": "display_popup",
		"arguments": map[string]any{
			"title": long,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized title")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
	if !strings.Contains(rerr.Message, "exceeds") {
		t.Errorf("error message = %q, want substring %q", rerr.Message, "exceeds")
	}
}

// TestHandle_DisplayPopup_RejectsBadSize pins the popupSizeRE shape on
// `width` / `height` / `x` / `y`. tmux only accepts a non-negative
// integer or the same followed by a percent sign, and refusing a
// stray "auto" / "%50" up front keeps the diagnostic close to the
// operator's mistake.
func TestHandle_DisplayPopup_RejectsBadSize(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name": "display_popup",
		"arguments": map[string]any{
			"width": "wide",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad width")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestHandle_DisplayPopup_RejectsRelativeStartDirectory pins the
// absolute-path policy on start_directory. A relative path would be
// resolved against tmux's own cwd, which is rarely what the caller
// meant — refusing it up front mirrors the session_create policy.
func TestHandle_DisplayPopup_RejectsRelativeStartDirectory(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name": "display_popup",
		"arguments": map[string]any{
			"start_directory": "relative/path",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for relative start_directory")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
	if !strings.Contains(rerr.Message, "absolute") {
		t.Errorf("error message = %q, want substring %q", rerr.Message, "absolute")
	}
}

// TestHandle_DisplayPopup_RejectsBadEnvKey pins the POSIX env-name
// policy: tmux's `-e KEY=VALUE` parser does not validate the key
// shape, so a stray `=` or whitespace would silently corrupt argv.
// The boundary refuses non-POSIX names up front.
func TestHandle_DisplayPopup_RejectsBadEnvKey(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name": "display_popup",
		"arguments": map[string]any{
			"env": map[string]any{"BAD KEY": "value"},
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad env key")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestHandle_DisplayPopup_HeadlessServerSurfacesCleanError pins the
// integration wire contract on the headless tmux server tmux-mcp owns:
// `tmux display-popup` *requires* a current client (it has nothing to
// draw on without one), so the call must fail with a structured error
// rather than hang or panic. The exact code depends on how far tmux
// got before giving up — a missing target that tmux noticed first
// surfaces as CodeSessionNotFound (-32000), and the more common
// "no current client" surface that tmux raises before it even checks
// the target maps to CodeInternal (-32603). Either path is a clean
// boundary error and signals to operators that the daemon has nothing
// to draw on.
//
// We anchor the daemon with a real session first so the controller
// hits the "server up" branch (a fresh controller has no socket file
// yet, which produces a different "error connecting" message that the
// boundary deliberately surfaces unchanged).
func TestHandle_DisplayPopup_HeadlessServerSurfacesCleanError(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dp_anchor", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "dp_anchor"},
			}))
	})

	params := mustJSON(t, map[string]any{
		"name": "display_popup",
		"arguments": map[string]any{
			"target": "definitely_missing_xyzzy",
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for unknown target on headless server, got result %#v", res)
	}
	// The two acceptable codes are CodeSessionNotFound (target was
	// noticed before the no-client surface) and CodeInternal (tmux
	// rejected with "no current client" before checking the target).
	// Either is a clean structured boundary error — the regression we
	// guard against is the call hanging or panicking.
	if rerr.Code != errs.CodeSessionNotFound && rerr.Code != errs.CodeInternal {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d) or CodeInternal (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, errs.CodeInternal, rerr.Message)
	}
}

// TestHandle_DisplayPopup_ListedInToolsAndStrictSchema confirms the
// init()-time registration actually wired display_popup into
// tools/list and that the schema locks additionalProperties:false so
// a typo in arguments fails fast against any spec-driven client.
func TestHandle_DisplayPopup_ListedInToolsAndStrictSchema(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] != "display_popup" {
			continue
		}
		schema, ok := def["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("display_popup inputSchema not a map: %#v", def["inputSchema"])
		}
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("display_popup additionalProperties = %v, want false",
				schema["additionalProperties"])
		}
		// Sanity-check the documented field set is present so a
		// future contributor cannot silently rename a schema key
		// without also touching this guard.
		props, _ := schema["properties"].(map[string]any)
		for _, want := range []string{
			"target", "title", "border_style", "border_lines",
			"start_directory", "env", "width", "height", "x", "y",
			"shell_command", "no_border", "close_on_exit",
			"close_on_zero_exit", "centered",
		} {
			if _, ok := props[want]; !ok {
				t.Errorf("display_popup schema missing property %q", want)
			}
		}
		return
	}
	t.Fatal("tools/list missing display_popup")
}

// TestHandle_DisplayPopup_NotInReadOnlyAllowlist pins the policy
// counterpart of the schema gates above: display_popup mutates client
// UI state, so it must be refused under -read-only. The companion
// readonly_test.go exercises the dispatcher gate end-to-end; this
// test asserts the same fact at the policy-table level so a future
// contributor flipping IsReadOnlyTool's source of truth has both
// pins fail in one PR.
func TestHandle_DisplayPopup_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("display_popup") {
		t.Fatal("IsReadOnlyTool(\"display_popup\") = true, want false (mutates client UI state)")
	}
}

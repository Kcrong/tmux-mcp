package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_SaveBuffer_DefaultDumpsMostRecent confirms the empty-name
// path resolves to the most-recently-added buffer, mirroring
// show_buffer's default. Both `name: ""` and the field-omitted form
// must resolve to the same buffer so the schema stays back-compat
// with clients that omit the field entirely.
func TestHandle_SaveBuffer_DefaultDumpsMostRecent(t *testing.T) {
	t.Parallel()
	tools, call, _ := bufferTestSetup(t)

	seedBuffer(t, tools, "", "first")
	seedBuffer(t, tools, "", "second")

	for _, args := range []map[string]any{
		{},
		{"name": ""},
	} {
		showText := extractText(t, call("save_buffer", args))
		var showObj struct {
			Name string `json:"name"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal([]byte(showText), &showObj); err != nil {
			t.Fatalf("decode save_buffer: %v\nbody=%s", err, showText)
		}
		if showObj.Data != "second" {
			t.Errorf("save_buffer default data = %q, want %q (args=%v)", showObj.Data, "second", args)
		}
		if showObj.Name != "" {
			t.Errorf("save_buffer default name echoed = %q, want empty (args=%v)", showObj.Name, args)
		}
	}
}

// TestHandle_SaveBuffer_NamedReturnsSpecific exercises the -b path
// through the dispatcher: seed two buffers, dump one by name, and
// confirm the contents round-trip exactly. A decoy buffer is added
// so the most-recent default would pick a different buffer — proves
// -b is honoured.
func TestHandle_SaveBuffer_NamedReturnsSpecific(t *testing.T) {
	t.Parallel()
	tools, call, _ := bufferTestSetup(t)

	const want = "the quick brown fox"
	seedBuffer(t, tools, "named_save", want)
	seedBuffer(t, tools, "", "decoy")

	showText := extractText(t, call("save_buffer", map[string]any{"name": "named_save"}))
	var showObj struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(showText), &showObj); err != nil {
		t.Fatalf("decode save_buffer: %v\nbody=%s", err, showText)
	}
	if showObj.Data != want {
		t.Errorf("save_buffer(named) data = %q, want %q", showObj.Data, want)
	}
	if showObj.Name != "named_save" {
		t.Errorf("save_buffer(named) echoed name = %q, want %q", showObj.Name, "named_save")
	}
}

// TestHandle_SaveBuffer_MissingMapsCode pins the wire contract for
// an unknown buffer name: the JSON-RPC error code must surface as
// CodeSessionNotFound (-32000) so MCP clients can branch on a stable
// code rather than the (version-specific) tmux stderr text. Mirrors
// show_buffer's contract so a client switching between the two read
// paths sees a stable signal.
func TestHandle_SaveBuffer_MissingMapsCode(t *testing.T) {
	t.Parallel()
	tools, _, ctx := bufferTestSetup(t)

	params := mustJSON(t, map[string]any{
		"name":      "save_buffer",
		"arguments": map[string]any{"name": "ghost_save_buffer_nonexistent"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for missing buffer, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_SaveBuffer_RejectsBadName locks the regex check on
// `name` so a stray quote/whitespace can't slip through to the tmux
// argv. The check runs before any tmux command, so the error must
// carry CodeInvalidParams (-32602).
func TestHandle_SaveBuffer_RejectsBadName(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "save_buffer",
		"arguments": map[string]any{"name": "bad name with spaces"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad buffer name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SaveBuffer_FitsUnderCap pins the small-payload happy
// path: a buffer comfortably under the configured
// MaxResponseBytes returns the body verbatim and never trips the
// pre-flight oversize check. Mirrors the steady-state scenario for
// agents that opted into the strict `error_on_truncation=true`
// default.
func TestHandle_SaveBuffer_FitsUnderCap(t *testing.T) {
	t.Parallel()
	tools, call, _ := bufferTestSetup(t)
	// Enough headroom for the default JSON envelope around a tiny
	// payload — set_buffer's 1 MiB ceiling is irrelevant here, the
	// only thing that matters is the cap is much bigger than the
	// candidate body.
	tools.MaxResponseBytes = 4096

	const want = "small payload"
	seedBuffer(t, tools, "small_save", want)

	showText := extractText(t, call("save_buffer", map[string]any{
		"name": "small_save",
	}))
	var showObj struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(showText), &showObj); err != nil {
		t.Fatalf("decode save_buffer: %v\nbody=%s", err, showText)
	}
	if showObj.Data != want {
		t.Errorf("save_buffer(small_save) data = %q, want %q", showObj.Data, want)
	}
}

// TestHandle_SaveBuffer_OverCapErrorsByDefault pins the load-bearing
// invariant of the new tool: when the marshalled payload would
// exceed the server's configured MaxResponseBytes cap and the
// caller has not opted out (`error_on_truncation` defaults to true),
// the handler returns the typed CodeOversizedResponse (-32010)
// directly — the dispatcher's framing-level guard is not the only
// safety net. This is the headline difference vs. show_buffer.
func TestHandle_SaveBuffer_OverCapErrorsByDefault(t *testing.T) {
	t.Parallel()
	tools, _, ctx := bufferTestSetup(t)
	// Tight cap so a modest 4 KiB payload trips the pre-flight check.
	// We deliberately set the cap below the buffer payload size; the
	// JSON envelope adds further bytes on top, but the cap-vs-payload
	// inequality alone is enough to land us on the oversize branch.
	tools.MaxResponseBytes = 256

	payload := strings.Repeat("xx", 2048) // 4096 bytes, well over 256.
	seedBuffer(t, tools, "big_save", payload)

	params := mustJSON(t, map[string]any{
		"name":      "save_buffer",
		"arguments": map[string]any{"name": "big_save"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected oversize error, got result %#v", res)
	}
	if rerr.Code != errs.CodeOversizedResponse {
		t.Fatalf("code = %d, want CodeOversizedResponse (%d), msg=%q",
			rerr.Code, errs.CodeOversizedResponse, rerr.Message)
	}
	if !strings.Contains(rerr.Message, "max-response-bytes") {
		t.Fatalf("error message %q must reference max-response-bytes", rerr.Message)
	}
}

// TestHandle_SaveBuffer_OverCapAllowedWhenOptedOut pins the inverse
// path: when the caller passes `error_on_truncation=false`, the
// handler ships the payload to the standard jsonBlock envelope and
// leaves cap enforcement to the dispatcher — the handler itself
// must not synthesise a CodeOversizedResponse. We assert success at
// the handler boundary; the dispatcher's after-the-fact replacement
// is exercised by jsonrpc_test.go.
func TestHandle_SaveBuffer_OverCapAllowedWhenOptedOut(t *testing.T) {
	t.Parallel()
	tools, call, _ := bufferTestSetup(t)
	tools.MaxResponseBytes = 256

	payload := strings.Repeat("xx", 2048)
	seedBuffer(t, tools, "big_save_optout", payload)

	showText := extractText(t, call("save_buffer", map[string]any{
		"name":                "big_save_optout",
		"error_on_truncation": false,
	}))
	var showObj struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(showText), &showObj); err != nil {
		t.Fatalf("decode save_buffer: %v\nbody=%s", err, showText)
	}
	if showObj.Data != payload {
		t.Fatalf("save_buffer(big_save_optout) data length = %d, want %d", len(showObj.Data), len(payload))
	}
}

// TestHandle_SaveBuffer_NoCapShipsWhole pins that the pre-flight
// check is skipped entirely when the operator has not armed
// -max-response-bytes (Tools.MaxResponseBytes <= 0). Mirrors the
// dispatcher's "<= 0 means uncapped" contract so an unconfigured
// deployment sees no behaviour change. The payload size is chosen
// to be large enough that a positive cap of 256 bytes (the value the
// over-cap test uses) would trip the gate — proving the only thing
// keeping this test green is the disabled cap, not the payload size.
func TestHandle_SaveBuffer_NoCapShipsWhole(t *testing.T) {
	t.Parallel()
	tools, call, _ := bufferTestSetup(t)
	// Explicitly leave MaxResponseBytes at zero; the dispatcher would
	// also leave the framing-level cap disabled in this configuration.
	tools.MaxResponseBytes = 0

	// 4 KiB is comfortably bigger than the 256-byte cap used in the
	// over-cap test, so a regression that defaulted MaxResponseBytes to
	// some small positive value would surface here.
	payload := strings.Repeat("yy", 2048)
	seedBuffer(t, tools, "uncapped_save", payload)

	showText := extractText(t, call("save_buffer", map[string]any{
		"name": "uncapped_save",
	}))
	var showObj struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(showText), &showObj); err != nil {
		t.Fatalf("decode save_buffer: %v\nbody=%s", err, showText)
	}
	if len(showObj.Data) != len(payload) {
		t.Fatalf("save_buffer uncapped data length = %d, want %d",
			len(showObj.Data), len(payload))
	}
}

// TestHandle_ToolsList_IncludesSaveBuffer makes sure tools/list
// advertises save_buffer so MCP clients can discover it via the
// schema endpoint.
func TestHandle_ToolsList_IncludesSaveBuffer(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "save_buffer" {
			return
		}
	}
	t.Fatal("tools/list missing save_buffer")
}

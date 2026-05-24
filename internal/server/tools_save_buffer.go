package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// saveBufferToolDefs registers `save_buffer` onto the dispatcher's
// tool surface. The schema mirrors `show_buffer` (optional `name`)
// plus the `error_on_truncation` knob a caller flips when they want
// the JSON-RPC layer to refuse oversize bodies up front instead of
// letting the dispatcher's framing-level
// [WithMaxResponseBytes] guard replace the response after the fact.
//
// Distinct from show_buffer in intent: callers reaching for
// save_buffer are signalling "I'm reading the canonical save-path —
// give me the whole payload or fail loudly". The default
// `error_on_truncation=true` keeps the contract honest by surfacing
// [errs.CodeOversizedResponse] (-32010) directly from the handler
// when the would-be reply exceeds the server's configured response
// ceiling — the caller can then retry with a smaller scope (e.g.
// pinning a different buffer) instead of receiving a silently-
// truncated body.
var saveBufferToolDefs = []map[string]any{
	{
		"name": "save_buffer",
		"description": "Return the raw text content of a tmux paste buffer via `tmux save-buffer - [-b NAME]`. " +
			"Semantically equivalent to `show_buffer` but signals \"this is the canonical save-path read\": " +
			"when `error_on_truncation` is true (the default) the handler returns " +
			"`-32010 oversized response` directly if the payload would not fit under the server's " +
			"configured `-max-response-bytes` cap, so a caller cannot silently receive a truncated body. " +
			"Omit `name` (or pass an empty string) to dump the most-recently-added buffer, matching the " +
			"tmux CLI default. When `name` is supplied, `tmux save-buffer -b <name> -` runs and the value " +
			"round-trips verbatim. Pair with `list_buffers` to discover the available names.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Optional buffer name; defaults to the most-recently-added buffer.",
				},
				"error_on_truncation": map[string]any{
					"type": "boolean",
					"description": "When true (the default), return -32010 oversized response if the payload " +
						"would exceed the server's configured -max-response-bytes cap, instead of letting " +
						"the framing layer replace the body after the fact. When false, the handler ships " +
						"the payload verbatim and leaves cap enforcement to the dispatcher.",
					"default": true,
				},
			},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register save_buffer onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from a single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, saveBufferToolDefs...)
}

// saveBuffer drives [tmuxctl.Controller.SaveBuffer] and serialises
// the result onto the standard `{"name": "...", "data": "..."}`
// envelope show_buffer also uses, so a client switching between the
// two read paths sees the same wire shape.
//
// The optional `name` is validated up front against the same
// regex/length policy the rest of the buffer-tool surface uses
// (see [validateBufferName]); a malformed value sees -32602 before
// any tmux command runs. An empty `name` resolves to
// `tmux save-buffer -` (no -b), dumping the most-recently-added
// buffer.
//
// The load-bearing difference vs. show_buffer is the
// `error_on_truncation` knob: when true (the default), the handler
// builds the candidate response, marshals it through the same
// envelope the dispatcher would write, and — when the resulting body
// would exceed the server's configured cap — returns
// [errs.ErrOversizedResponse] so the caller sees -32010 directly
// instead of relying on the dispatcher's after-the-fact replacement.
// When the cap is disabled (Tools.MaxResponseBytes <= 0) the check
// is skipped, mirroring the dispatcher's "no cap, no replacement"
// fast-path. error_on_truncation=false ships the payload verbatim
// and leaves cap enforcement to the framing layer, exactly as
// show_buffer does today.
func (t *Tools) saveBuffer(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	args := struct {
		Name              string `json:"name"`
		ErrorOnTruncation *bool  `json:"error_on_truncation"`
	}{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("save_buffer: %v", err)
		}
	}
	if rerr := validateBufferName(args.Name); rerr != nil {
		return nil, invalidParams("save_buffer: %s", rerr.Message)
	}
	// Default error_on_truncation=true. Pointer-typed unmarshal lets
	// us tell "field absent" from "field present and false" without a
	// second decode pass.
	errorOnTruncation := true
	if args.ErrorOnTruncation != nil {
		errorOnTruncation = *args.ErrorOnTruncation
	}
	body, err := t.Ctl.SaveBuffer(ctx, args.Name)
	if err != nil {
		return nil, internalError(fmt.Errorf("save_buffer: %w", err))
	}
	payload := map[string]any{
		"name": args.Name,
		"data": body,
	}
	// Pre-flight the response size when both the operator armed
	// -max-response-bytes (Tools.MaxResponseBytes > 0) and the caller
	// asked us to enforce the cap up front (error_on_truncation=true,
	// the default). We measure the marshalled body the dispatcher
	// would actually write — the same calculation
	// [oversizeRerr] performs in jsonrpc.go — so the two layers agree
	// on the threshold. When the limit is disabled or the caller opted
	// out we fall through to the standard jsonBlock path and let the
	// dispatcher's framing-level guard handle the rare oversize case
	// (it will replace the body with the same -32010 typed error, just
	// after the fact instead of pre-emptively).
	if errorOnTruncation && t.MaxResponseBytes > 0 {
		buf, mErr := json.Marshal(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": mustMarshalString(payload)},
			},
		})
		if mErr != nil {
			return nil, internalError(fmt.Errorf("save_buffer: %w", mErr))
		}
		// The dispatcher measures the entire JSON-RPC envelope
		// (id + jsonrpc + result + …) when it computes the cap
		// against -max-response-bytes; here we only have the result
		// payload, so we conservatively reject when the result alone
		// already breaches the limit. Slightly stricter than the
		// dispatcher (which accepts a result whose envelope also
		// fits) but it's the right direction — we never let a
		// would-be-oversize body escape the handler — and it keeps
		// the check independent of dispatcher-internal framing
		// decisions like the request id width.
		if int64(len(buf)) > t.MaxResponseBytes {
			return nil, internalError(fmt.Errorf(
				"save_buffer: response body %d bytes exceeds max-response-bytes %d: %w",
				len(buf), t.MaxResponseBytes, errs.ErrOversizedResponse,
			))
		}
	}
	return jsonBlock(payload)
}

// mustMarshalString is the shape jsonBlock encodes onto the
// `{type: text, text: <json-string>}` envelope. We re-implement the
// single line here so the size pre-flight in [Tools.saveBuffer]
// can measure the exact bytes the dispatcher would write without
// going through jsonBlock (which discards the *rpcError on the
// happy-path). Marshal of a map[string]any with string-keyed values
// cannot fail in practice — every value is already a JSON-safe
// primitive — so panicking on the unexpected error keeps the
// caller code clean. A real failure would surface from json.Marshal
// in the caller's branch above anyway.
func mustMarshalString(v any) string {
	buf, err := json.Marshal(v)
	if err != nil {
		// json.Marshal of a string-keyed map of strings/ints/bools
		// genuinely cannot fail at runtime; if the stdlib ever
		// regresses on that we want the panic so tests catch it
		// rather than emitting a half-formed body.
		panic(fmt.Sprintf("save_buffer: marshal: %v", err))
	}
	return string(buf)
}

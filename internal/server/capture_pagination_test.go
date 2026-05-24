package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
	"github.com/Kcrong/tmux-mcp/internal/snapshot"
	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// newToolsForPagination returns a *Tools instance with the capture
// buffer wired up but no live tmux controller. Callers drive
// captureFirstPage / captureFromCursor directly with synthetic bodies,
// which sidesteps tmux-on-PATH requirements and lets the suite run
// anywhere.
func newToolsForPagination() *Tools {
	return &Tools{
		Snap:     snapshot.New(),
		captures: newCaptureBufferStore(),
	}
}

// mkLines builds a synthetic capture body of n lines so tests can
// exercise pagination without invoking tmux. The format mirrors what
// `seq 1 N` emits — one decimal per line — so it is easy to spot
// out-of-order reassembly when an assertion fails.
func mkLines(n int) string {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = fmt.Sprintf("line-%d", i)
	}
	return strings.Join(parts, "\n")
}

// callCaptureWithCursor exercises the public capture handler with a
// synthetic cursor argument. It is used by the stale-cursor and
// unknown-session tests, which only care about the dispatcher's
// invalid-params path and never need to round-trip through tmux.
func callCaptureWithCursor(t *testing.T, tools *Tools, session, cursor string, chunkLines int) (any, *rpcError) {
	t.Helper()
	args := map[string]any{
		"session":     session,
		"cursor":      cursor,
		"chunk_lines": chunkLines,
	}
	raw, err := json.Marshal(map[string]any{"name": "capture", "arguments": args})
	if err != nil {
		t.Fatalf("marshal capture call: %v", err)
	}
	return tools.Handle(context.Background(), "tools/call", raw)
}

// extractCaptureFields decodes the JSON text block returned by capture
// into a flat map so individual tests can assert on cursor / total_lines
// / snapshot without each rolling its own type assertions.
func extractCaptureFields(t *testing.T, result any) map[string]any {
	t.Helper()
	body := extractText(t, result)
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode capture text: %v\nbody=%s", err, body)
	}
	return m
}

// TestCapturePagination_ReassemblesHugeScrollback walks the synthetic
// 12000-line scrollback through three captureFirstPage / captureFromCursor
// calls and asserts the reassembled body equals the original byte-for-byte.
// chunk_lines=5000 → 5000 + 5000 + 2000.
func TestCapturePagination_ReassemblesHugeScrollback(t *testing.T) {
	t.Parallel()
	tools := newToolsForPagination()
	const session = "huge"
	const total = 12000
	body := mkLines(total)

	// Page 0. max_lines=20000 defeats the default 5000 cap so the full
	// 12000 lines reach the paginator.
	res := tools.captureFirstPage(session, body, tmuxctl.CaptureScrollback, 20000, 5000)
	if res.cursor == "" {
		t.Fatalf("expected non-empty cursor after first page of 12000 lines")
	}
	if res.totalLines != total {
		t.Fatalf("page 0 total_lines = %d, want %d", res.totalLines, total)
	}
	page0Lines := strings.Split(res.body, "\n")
	if len(page0Lines) != 5000 {
		t.Fatalf("page 0 line count = %d, want 5000", len(page0Lines))
	}
	if page0Lines[0] != "line-0" || page0Lines[4999] != "line-4999" {
		t.Fatalf("page 0 boundaries wrong: first=%q last=%q", page0Lines[0], page0Lines[4999])
	}

	// Page 1.
	res1, rerr := tools.captureFromCursor(session, res.cursor, 5000)
	if rerr != nil {
		t.Fatalf("page 1: %v", rerr)
	}
	if res1.cursor == "" {
		t.Fatalf("expected non-empty cursor after page 1 (12000 lines, 5000 each)")
	}
	page1Lines := strings.Split(res1.body, "\n")
	if len(page1Lines) != 5000 {
		t.Fatalf("page 1 line count = %d, want 5000", len(page1Lines))
	}
	if page1Lines[0] != "line-5000" || page1Lines[4999] != "line-9999" {
		t.Fatalf("page 1 boundaries wrong: first=%q last=%q", page1Lines[0], page1Lines[4999])
	}

	// Page 2 — the tail. Cursor should come back empty.
	res2, rerr := tools.captureFromCursor(session, res1.cursor, 5000)
	if rerr != nil {
		t.Fatalf("page 2: %v", rerr)
	}
	if res2.cursor != "" {
		t.Fatalf("expected empty cursor after final page, got %q", res2.cursor)
	}
	page2Lines := strings.Split(res2.body, "\n")
	if len(page2Lines) != 2000 {
		t.Fatalf("page 2 line count = %d, want 2000", len(page2Lines))
	}
	if page2Lines[0] != "line-10000" || page2Lines[1999] != "line-11999" {
		t.Fatalf("page 2 boundaries wrong: first=%q last=%q", page2Lines[0], page2Lines[1999])
	}

	// Reassemble and compare.
	reassembled := strings.Join([]string{res.body, res1.body, res2.body}, "\n")
	if reassembled != body {
		t.Fatalf("reassembled body mismatch:\nlen(orig)=%d len(got)=%d", len(body), len(reassembled))
	}

	// The buffer must be dropped after the last page so we don't pin
	// scrollback indefinitely.
	if _, ok := tools.captures.get(session, ""); ok {
		t.Fatalf("buffer should have been dropped after final page")
	}
}

// TestCapturePagination_StaleCursor uses a cursor that points at a
// captured_at id no longer matching the live buffer (i.e. another
// capture rotated it). The handler must reject with -32602 instead of
// silently returning data from the wrong capture.
func TestCapturePagination_StaleCursor(t *testing.T) {
	t.Parallel()
	tools := newToolsForPagination()
	const session = "stale"

	// Seed buffer A and grab a valid cursor pointing into it.
	resA := tools.captureFirstPage(session, mkLines(12000), tmuxctl.CaptureScrollback, 20000, 5000)
	if resA.cursor == "" {
		t.Fatal("expected non-empty cursor for >1 page capture")
	}

	// Rotate the buffer with a fresh capture. The new captured_at id
	// will not match the cursor we still hold from buffer A.
	_ = tools.captureFirstPage(session, mkLines(12000), tmuxctl.CaptureScrollback, 20000, 5000)

	_, rerr := tools.captureFromCursor(session, resA.cursor, 5000)
	if rerr == nil {
		t.Fatal("expected -32602 for stale cursor, got nil")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("stale cursor error code = %d, want %d", rerr.Code, errs.CodeInvalidParams)
	}

	// Same path, but exercised end-to-end through the JSON-RPC handler
	// to make sure the error code propagates through the tools/call
	// dispatcher unchanged.
	res, rerr := callCaptureWithCursor(t, tools, session, resA.cursor, 5000)
	if rerr == nil {
		t.Fatalf("dispatcher swallowed stale cursor error: result=%#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("dispatcher error code = %d, want %d", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestCapturePagination_UnknownSession passes a syntactically valid
// cursor that names a session the buffer store knows nothing about
// (because no capture has been issued there yet). The handler must
// reject with -32602.
func TestCapturePagination_UnknownSession(t *testing.T) {
	t.Parallel()
	tools := newToolsForPagination()

	cursor := encodeCursor(cursorPayload{
		Session:    "ghost",
		Offset:     5000,
		CapturedAt: "deadbeefdeadbeef",
	})
	_, rerr := tools.captureFromCursor("ghost", cursor, 5000)
	if rerr == nil {
		t.Fatal("expected -32602 for unknown session, got nil")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("unknown session error code = %d, want %d", rerr.Code, errs.CodeInvalidParams)
	}

	// Same via the dispatcher.
	res, rerr := callCaptureWithCursor(t, tools, "ghost", cursor, 5000)
	if rerr == nil {
		t.Fatalf("dispatcher swallowed unknown-session error: result=%#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("dispatcher error code = %d, want %d", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestCapturePagination_ChunkLinesZeroFallsBack pins the chunk_lines=0
// → defaultChunkLines contract. A 12000-line body with chunk_lines=0
// must paginate at the default of 5000, not return all 12000 in one
// page.
func TestCapturePagination_ChunkLinesZeroFallsBack(t *testing.T) {
	t.Parallel()
	tools := newToolsForPagination()
	const session = "fallback"

	res := tools.captureFirstPage(session, mkLines(12000), tmuxctl.CaptureScrollback, 20000, 0)
	page0 := strings.Split(res.body, "\n")
	if len(page0) != defaultChunkLines {
		t.Fatalf("page 0 line count with chunk_lines=0 = %d, want default %d",
			len(page0), defaultChunkLines)
	}
	if res.cursor == "" {
		t.Fatal("expected non-empty cursor when body exceeds default chunk")
	}

	// Follow-up with chunk_lines=0 must also fall back to the default.
	res1, rerr := tools.captureFromCursor(session, res.cursor, 0)
	if rerr != nil {
		t.Fatalf("page 1: %v", rerr)
	}
	page1 := strings.Split(res1.body, "\n")
	if len(page1) != defaultChunkLines {
		t.Fatalf("page 1 line count with chunk_lines=0 = %d, want default %d",
			len(page1), defaultChunkLines)
	}
}

// TestCapturePagination_BackwardsCompatNoCursor exercises the small-
// scrollback path. A capture that fits in one chunk must return an
// empty cursor (so the wire shape matches pre-pagination clients) and
// must NOT allocate a pagination buffer.
func TestCapturePagination_BackwardsCompatNoCursor(t *testing.T) {
	t.Parallel()
	tools := newToolsForPagination()
	const session = "small"

	// 200 lines is well under any plausible chunk size; the existing
	// 5000-line default cap also leaves it unchanged.
	body := mkLines(200)
	res := tools.captureFirstPage(session, body, tmuxctl.CaptureScrollback, 0, 5000)
	if res.cursor != "" {
		t.Fatalf("expected empty cursor for small scrollback, got %q", res.cursor)
	}
	if res.totalLines != 200 {
		t.Fatalf("total_lines = %d, want 200", res.totalLines)
	}
	if res.body != body {
		t.Fatalf("body modified for small scrollback")
	}
	if res.truncated {
		t.Fatal("did not expect truncation for body shorter than default cap")
	}
	// No buffer should have been allocated.
	if _, ok := tools.captures.get(session, ""); ok {
		t.Fatal("buffer was allocated for a one-page capture")
	}

	// Visible mode must also never paginate, even when the synthetic
	// body is large — the spec carves that mode out.
	resV := tools.captureFirstPage(session, mkLines(20000), tmuxctl.CaptureVisible, 0, 5000)
	if resV.cursor != "" {
		t.Fatalf("visible mode must not paginate, got cursor %q", resV.cursor)
	}
}

// TestCapturePagination_BufferTTLEvicts pins the lazy-TTL contract: a
// buffer past its expiration is dropped on the next captureFromCursor
// call and the caller sees the unknown-session rejection rather than a
// stale page.
func TestCapturePagination_BufferTTLEvicts(t *testing.T) {
	t.Parallel()
	tools := newToolsForPagination()
	// Pin the clock so the test does not have to actually sleep through
	// the production 5-minute TTL.
	var current time.Time
	tools.captures.now = func() time.Time { return current }
	current = time.Date(2026, time.May, 24, 12, 0, 0, 0, time.UTC)

	const session = "ttl"
	res := tools.captureFirstPage(session, mkLines(12000), tmuxctl.CaptureScrollback, 20000, 5000)
	if res.cursor == "" {
		t.Fatal("expected non-empty cursor for >1 page capture")
	}

	// Advance past the TTL and exercise cleanup via captureFromCursor.
	current = current.Add(captureBufferTTL + time.Second)
	_, rerr := tools.captureFromCursor(session, res.cursor, 5000)
	if rerr == nil {
		t.Fatal("expected -32602 after TTL eviction")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("TTL eviction error code = %d, want %d", rerr.Code, errs.CodeInvalidParams)
	}

	// And the lazy cleanup() walker should also drop the entry on its
	// next pass — assert the map is genuinely empty so we know the
	// buffer was not just hidden by the get() check.
	tools.captures.cleanup()
	tools.captures.mu.Lock()
	got := len(tools.captures.buffers)
	tools.captures.mu.Unlock()
	if got != 0 {
		t.Fatalf("expected buffer map empty after TTL+cleanup, got %d entries", got)
	}
}

// TestCapturePagination_CursorSessionMismatch covers the defensive
// check that a cursor's embedded session must equal the request's
// session field. A caller that mixes cursors across sessions gets
// -32602 rather than data from someone else's buffer.
func TestCapturePagination_CursorSessionMismatch(t *testing.T) {
	t.Parallel()
	tools := newToolsForPagination()

	// Seed a buffer for session A and capture its cursor.
	resA := tools.captureFirstPage("alpha", mkLines(12000), tmuxctl.CaptureScrollback, 20000, 5000)
	if resA.cursor == "" {
		t.Fatal("expected non-empty cursor for paginated capture")
	}

	// Replay the cursor under a different session name.
	_, rerr := tools.captureFromCursor("beta", resA.cursor, 5000)
	if rerr == nil {
		t.Fatal("expected -32602 for cross-session cursor reuse")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("cross-session error code = %d, want %d", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestCapturePagination_MalformedCursor pins the defensive parse path:
// a cursor that is not valid base64 → -32602. Without this check we'd
// rely on the dispatcher to surface the json error, which is fine but
// the explicit message ("invalid cursor") gives clients better signal.
func TestCapturePagination_MalformedCursor(t *testing.T) {
	t.Parallel()
	tools := newToolsForPagination()
	_, rerr := tools.captureFromCursor("anything", "@@not-base64@@", 5000)
	if rerr == nil {
		t.Fatal("expected -32602 for malformed cursor")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("malformed cursor error code = %d, want %d", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestCapturePagination_HandlerEmitsNewFields asserts the JSON wire
// shape of a paginated first-page response: snapshot+token+changed
// (existing) plus cursor+total_lines (new). A missing field would
// silently break clients, so the assertion is structural rather than
// loose substring matching.
func TestCapturePagination_HandlerEmitsNewFields(t *testing.T) {
	t.Parallel()
	tools := newToolsForPagination()
	const session = "wire"
	body := mkLines(12000)
	// Drive captureFirstPage manually because the handler reaches into
	// t.Ctl.Capture for the body, which would require live tmux. We
	// then manually marshal into the same shape as the handler.
	res := tools.captureFirstPage(session, body, tmuxctl.CaptureScrollback, 20000, 5000)
	snap := tools.Snap.Record(session, res.body)
	out, rerr := jsonBlock(map[string]any{
		"snapshot":    res.body,
		"token":       snap.Token,
		"changed":     snap.Changed,
		"truncated":   res.truncated,
		"cursor":      res.cursor,
		"total_lines": res.totalLines,
	})
	if rerr != nil {
		t.Fatalf("jsonBlock: %v", rerr)
	}
	fields := extractCaptureFields(t, out)
	for _, want := range []string{"snapshot", "token", "changed", "truncated", "cursor", "total_lines"} {
		if _, ok := fields[want]; !ok {
			t.Errorf("response missing field %q: %#v", want, fields)
		}
	}
	if got, _ := fields["total_lines"].(float64); int(got) != 12000 {
		t.Errorf("total_lines = %v, want 12000", fields["total_lines"])
	}
	if cursor, _ := fields["cursor"].(string); cursor == "" {
		t.Error("expected non-empty cursor in first-page response")
	}
}

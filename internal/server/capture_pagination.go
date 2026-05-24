package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// defaultChunkLines is the default chunk size for cursor-based pagination
// of `capture` results. It mirrors defaultScrollbackMaxLines so a default
// scrollback capture (capped at 5000) fits in a single page and the new
// cursor field is empty — preserving back-compat for callers that never
// touch chunk_lines or cursor.
const defaultChunkLines = 5000

// captureBufferTTL bounds how long a per-session pagination buffer
// survives before it is evicted. tmux scrollback can be many MB, so we
// drop the snapshot promptly once the caller stops paging through it.
// Cleanup is lazy (driven by the next capture call) — no goroutine.
const captureBufferTTL = 5 * time.Minute

// pendingCapture holds the body of a single scrollback capture that the
// server has chunked for cursor-based pagination. The body is stored as
// a pre-split slice so each follow-up page is an O(1) slice op.
type pendingCapture struct {
	// id is a per-capture random handle. It is embedded in every cursor
	// returned for this buffer and re-checked on follow-up calls so a
	// caller cannot resume into a buffer that was rotated by a newer
	// capture on the same session.
	id string
	// lines is the body split on '\n'. Splitting eagerly makes follow-up
	// pages a slice instead of a re-walk.
	lines []string
	// total is len(lines) at capture time. We surface it on every page so
	// the caller can render a progress bar.
	total int
	// expiresAt is the wall-clock deadline after which this buffer is
	// considered evicted. Lazy cleanup compares against time.Now() in the
	// next capture call; we never spawn a goroutine for it.
	expiresAt time.Time
}

// captureBufferStore is the per-Tools cache of pending captures keyed by
// session name. It is safe for concurrent access from the JSON-RPC
// dispatcher's goroutines.
type captureBufferStore struct {
	mu      sync.Mutex
	buffers map[string]*pendingCapture
	ttl     time.Duration
	// now and newID are seams for tests. In production they default to
	// time.Now and a crypto/rand-backed hex id; tests can swap them to
	// pin behaviour without touching wall-clock or kernel entropy.
	now   func() time.Time
	newID func() string
}

// newCaptureBufferStore returns a buffer keyed by session name with the
// production-default clock and id generator wired in.
func newCaptureBufferStore() *captureBufferStore {
	return &captureBufferStore{
		buffers: map[string]*pendingCapture{},
		ttl:     captureBufferTTL,
		now:     time.Now,
		newID:   newCaptureID,
	}
}

// newCaptureID returns 16 hex chars (8 random bytes) suitable for tagging
// a pending-capture buffer. crypto/rand.Read effectively cannot fail on
// supported platforms; falling back to a fixed marker keeps the server
// non-fatal if it ever does.
func newCaptureID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// put stores a fresh buffer for session, evicting any existing entry,
// and returns the assigned capture id. The TTL clock starts now.
//
// A nil receiver is tolerated: a few tests construct *Tools as a bare
// struct literal (e.g. "validation should fail before tmux is touched")
// and would otherwise need to know about pagination internals. In that
// case we silently no-op and return an empty id; callers downstream
// branch on the empty id where it matters.
func (s *captureBufferStore) put(session string, lines []string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.newID()
	s.buffers[session] = &pendingCapture{
		id:        id,
		lines:     lines,
		total:     len(lines),
		expiresAt: s.now().Add(s.ttl),
	}
	return id
}

// get returns the buffer for session iff it exists, has not expired,
// and its id matches wantID. Anything else (no buffer, expired, id
// mismatch) is reported as a lookup failure so the caller can map it
// to -32602.
//
// A successful get also refreshes expiresAt so an active pagination
// session is not evicted mid-stream. A nil receiver is tolerated for
// the same reason as [put].
func (s *captureBufferStore) get(session, wantID string) (*pendingCapture, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	buf, ok := s.buffers[session]
	if !ok {
		return nil, false
	}
	now := s.now()
	if now.After(buf.expiresAt) {
		delete(s.buffers, session)
		return nil, false
	}
	if buf.id != wantID {
		return nil, false
	}
	buf.expiresAt = now.Add(s.ttl)
	return buf, true
}

// drop removes any buffer associated with session. Used after the last
// page is delivered so we don't pin tmux scrollback past its useful
// life. A nil receiver is tolerated for the same reason as [put].
func (s *captureBufferStore) drop(session string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.buffers, session)
}

// cleanup walks the map once and drops every buffer past its TTL. The
// dispatcher calls this opportunistically from `capture` so we never
// need a background goroutine. A nil receiver is tolerated for the
// same reason as [put].
func (s *captureBufferStore) cleanup() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, b := range s.buffers {
		if now.After(b.expiresAt) {
			delete(s.buffers, k)
		}
	}
}

// cursorPayload is the shape encoded inside an opaque cursor token. It
// is embedded base64'd so callers cannot meaningfully tamper with it
// (the server validates every field on the way back in).
type cursorPayload struct {
	Session    string `json:"session"`
	Offset     int    `json:"offset"`
	CapturedAt string `json:"captured_at"`
}

// encodeCursor serialises a cursor payload to its opaque wire form.
// Callers treat the result as an opaque blob and pass it back unchanged.
func encodeCursor(p cursorPayload) string {
	buf, err := json.Marshal(p)
	if err != nil {
		// json.Marshal of a fixed shape with string/int fields cannot
		// fail; returning empty here keeps the type signature simple.
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// decodeCursor parses an opaque cursor token back into its payload.
// Returns an error suitable for invalidParams when the cursor is not
// valid base64 or does not decode to the expected JSON shape.
func decodeCursor(s string) (cursorPayload, error) {
	var p cursorPayload
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	return p, nil
}

// resolveChunkLines applies the chunk_lines default. A non-positive
// value (zero or negative — JSON minimum=0 means clients can only
// supply zero) falls back to defaultChunkLines so a caller that knows
// nothing about the new field still gets sensible behaviour.
func resolveChunkLines(n int) int {
	if n <= 0 {
		return defaultChunkLines
	}
	return n
}

// splitForPagination splits a capture body on '\n', preserving the
// trailing empty token that strings.Split emits for bodies ending in a
// newline so reassembling a paged capture round-trips byte-for-byte.
func splitForPagination(body string) []string {
	return strings.Split(body, "\n")
}

// chunkResult is the data the capture handler emits for a single page,
// independent of the textBlock/jsonBlock plumbing in tools.go.
type chunkResult struct {
	body       string
	cursor     string
	totalLines int
	truncated  bool
}

// pageBody slices body[off:off+chunk_lines] (clamped) and returns the
// resulting page plus the next cursor. cursor is empty when this is the
// last page; the caller drops the buffer in that case.
func pageBody(session, captureID string, lines []string, offset, chunkLines int) (page string, nextCursor string, lastPage bool) {
	end := offset + chunkLines
	if end >= len(lines) {
		end = len(lines)
		page = strings.Join(lines[offset:end], "\n")
		return page, "", true
	}
	page = strings.Join(lines[offset:end], "\n")
	nextCursor = encodeCursor(cursorPayload{
		Session:    session,
		Offset:     end,
		CapturedAt: captureID,
	})
	return page, nextCursor, false
}

// captureFirstPage is the post-capture pipeline: apply the existing
// max_lines cap, then split into pages of chunkLines. When the body
// fits in one page the buffer is not allocated and the returned cursor
// is empty — preserving the pre-pagination wire shape for back-compat.
func (t *Tools) captureFirstPage(session, body string, mode tmuxctl.CaptureMode, maxLines, chunkLines int) chunkResult {
	body, truncated := capCaptureBody(body, mode, maxLines)
	chunkLines = resolveChunkLines(chunkLines)
	lines := splitForPagination(body)
	total := len(lines)
	if mode != tmuxctl.CaptureScrollback || total <= chunkLines {
		// Visible mode is bounded by the terminal anyway, and a
		// scrollback capture small enough to fit in one page never
		// allocates a pagination buffer.
		return chunkResult{
			body:       body,
			cursor:     "",
			totalLines: total,
			truncated:  truncated,
		}
	}
	// Need to paginate. Stash the full body so follow-ups read from it.
	id := t.captures.put(session, lines)
	page, cursor, _ := pageBody(session, id, lines, 0, chunkLines)
	return chunkResult{
		body:       page,
		cursor:     cursor,
		totalLines: total,
		truncated:  truncated,
	}
}

// captureFromCursor serves a follow-up page. It rejects cursors whose
// session field disagrees with the request, cursors whose captured_at
// no longer matches the live buffer (the buffer was rotated by a newer
// capture), and cursors whose buffer has aged out. Each rejection maps
// to invalidParams (-32602) at the call site.
func (t *Tools) captureFromCursor(session, cursorTok string, chunkLines int) (chunkResult, *rpcError) {
	p, err := decodeCursor(cursorTok)
	if err != nil {
		return chunkResult{}, invalidParams("capture: invalid cursor: %v", err)
	}
	if p.Session != session {
		return chunkResult{}, invalidParams("capture: cursor session %q does not match request session %q", p.Session, session)
	}
	if p.Offset < 0 {
		return chunkResult{}, invalidParams("capture: cursor offset %d is negative", p.Offset)
	}
	buf, ok := t.captures.get(session, p.CapturedAt)
	if !ok {
		return chunkResult{}, invalidParams("capture: cursor is stale or for an unknown session")
	}
	if p.Offset > buf.total {
		return chunkResult{}, invalidParams("capture: cursor offset %d exceeds total %d", p.Offset, buf.total)
	}
	chunkLines = resolveChunkLines(chunkLines)
	page, nextCursor, last := pageBody(session, buf.id, buf.lines, p.Offset, chunkLines)
	if last {
		// Drop the buffer once the last page is delivered so we don't
		// pin tmux scrollback indefinitely. The TTL is the safety valve
		// when callers abandon a pagination mid-stream.
		t.captures.drop(session)
	}
	return chunkResult{
		body:       page,
		cursor:     nextCursor,
		totalLines: buf.total,
		// truncated is a property of the original capture; for follow-up
		// pages it does not apply — the value was reported on page 0.
		truncated: false,
	}, nil
}

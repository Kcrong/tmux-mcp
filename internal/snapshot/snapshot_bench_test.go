package snapshot

import (
	"strconv"
	"strings"
	"testing"
)

// BenchmarkStore_Record measures the cost of recording a fresh capture
// for an existing session. The body is non-trivial (~2.4 KiB) so that
// hashing dominates over map bookkeeping, matching real captures from
// scrollback. Each iteration tweaks the body so we never hit the
// "identical body" fast path and exercise the full hash + write.
func BenchmarkStore_Record(b *testing.B) {
	s := New()
	base := strings.Repeat("hello world\n", 200)
	// Prime the session so we measure the steady-state path, not the
	// first-record branch.
	s.Record("session", base)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Record("session", base+strconv.Itoa(i)+"\n")
	}
}

// BenchmarkStore_DiffSince measures the dominant call from the
// snapshot tool: hand back the per-line diff for a small change against
// a previous token. The prior body is ~2.5 KiB and the new body adds
// exactly one line, so this exercises the common "tail change" path.
func BenchmarkStore_DiffSince(b *testing.B) {
	s := New()
	body := strings.Repeat("line\n", 500)
	first := s.Record("session", body)
	body2 := body + "added\n"
	// Record once so the prior token resolves on every iteration.
	_, _ = s.DiffSince("session", first.Token, body2)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Use the original token; matchPrior keeps both current and
		// prior, so this resolves to a real diff every iteration.
		_, _ = s.DiffSince("session", first.Token, body2)
	}
}

// BenchmarkStore_DiffSince_FullReset covers the slower path where the
// caller's token is unknown and every line in the new body is reported
// as added. This is the worst-case allocation path.
func BenchmarkStore_DiffSince_FullReset(b *testing.B) {
	s := New()
	body := strings.Repeat("line\n", 500)
	_ = s.Record("session", body)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.DiffSince("session", "deadbeefdeadbeef", body)
	}
}

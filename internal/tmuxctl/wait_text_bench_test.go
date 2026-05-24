package tmuxctl

import (
	"regexp"
	"strings"
	"testing"
)

// BenchmarkWaitForText_Match drives the regex compile+match path that
// (*Controller).WaitForText runs on every poll iteration. We cannot
// exercise the full WaitForText loop without a live tmux server, but
// the only adversarial / variable input is the user pattern matched
// against the captured pane body — and that is exactly what is hot when
// timeouts span many polls. Compiling once outside the timer mirrors
// the production code which compiles once before the loop.
func BenchmarkWaitForText_Match(b *testing.B) {
	pattern := `READY-\d+`
	haystack := strings.Repeat("waiting...\n", 200) + "READY-42\n"
	re := regexp.MustCompile(pattern)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if m := re.FindString(haystack); m == "" {
			b.Fatal("expected match")
		}
	}
}

// BenchmarkWaitForText_NoMatch covers the dominant case during a long
// wait: we poll repeatedly and the pattern is not yet visible. Misses
// must remain cheap or `WaitForText` becomes CPU-bound on long timeouts.
func BenchmarkWaitForText_NoMatch(b *testing.B) {
	pattern := `READY-\d+`
	haystack := strings.Repeat("waiting...\n", 200)
	re := regexp.MustCompile(pattern)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = re.FindString(haystack)
	}
}

// BenchmarkWaitForText_Compile pins the per-call regex compile cost
// that WaitForText pays once before each polling loop. A regression
// that moves Compile inside the loop would be visible here as a sudden
// allocation increase relative to the steady-state Match benchmarks.
func BenchmarkWaitForText_Compile(b *testing.B) {
	pattern := `READY-\d+`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := regexp.Compile(pattern); err != nil {
			b.Fatal(err)
		}
	}
}

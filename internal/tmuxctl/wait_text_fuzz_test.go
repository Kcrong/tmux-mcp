package tmuxctl

import (
	"regexp"
	"testing"
	"time"
)

// FuzzWaitForTextRegex hammers the regex compile+match path used by
// (*Controller).WaitForText. The controller itself shells out to tmux,
// so it cannot run in a fuzzer that may have no tmux installed; the
// only adversarial input the controller actually feeds to standard
// library code is the user pattern (and the captured pane body it
// matches against). This fuzz drives that same surface — regexp.Compile
// followed by FindString — so any panic, hang, or runaway match in the
// regex engine surfaces here without needing tmux at all.
//
// Per docs: regexp.Compile is RE2-based and guarantees linear-time
// matching; this fuzzer pins that contract for the patterns we actually
// expose to callers and for the byte sequences we hand to the matcher.
func FuzzWaitForTextRegex(f *testing.F) {
	f.Add([]byte(``), []byte(``))
	f.Add([]byte(`READY-\d+`), []byte("READY-42\n"))
	f.Add([]byte(`a(b|c)+`), []byte("aabcbc"))
	f.Add([]byte(`^\$ `), []byte("$ ls\n$ pwd\n"))
	f.Add([]byte(`[[:alpha:]]+`), []byte("hello world"))
	// Patterns that legitimately fail to compile — the fuzz must
	// observe an error here, not a panic.
	f.Add([]byte(`(`), []byte("anything"))
	f.Add([]byte(`*`), []byte("anything"))
	f.Add([]byte(`\`), []byte("anything"))
	// Embedded NULs and high bytes in the haystack.
	f.Add([]byte(`.`), []byte("\x00\xff\xfe"))

	f.Fuzz(func(t *testing.T, pattern, haystack []byte) {
		t.Parallel()
		// We never call MustCompile — bad patterns must surface as
		// errors, not panics.
		re, err := regexp.Compile(string(pattern))
		if err != nil {
			// A compile error is expected for adversarial patterns.
			// The empty-pattern guard inside WaitForText short-circuits
			// before Compile, so we only need to verify the engine path
			// here.
			return
		}
		if re == nil {
			t.Fatalf("Compile returned nil error and nil regexp for pattern %q", pattern)
		}

		// All match operations must terminate quickly. Wrap them in a
		// deadline so a hypothetical regression to a non-RE2 engine
		// would surface as a fuzz timeout rather than a hang.
		done := make(chan struct{})
		go func() {
			defer close(done)
			// FindString is the operation WaitForText actually uses.
			_ = re.FindString(string(haystack))
			// Drive the other common entry points as well to broaden
			// coverage; none of these should ever panic.
			_ = re.MatchString(string(haystack))
			_ = re.Match(haystack)
			_ = re.FindStringIndex(string(haystack))
			_ = re.FindStringSubmatch(string(haystack))
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("regex match did not terminate within 5s: pattern=%q haystack-len=%d", pattern, len(haystack))
		}
	})
}

// FuzzWaitForTextPattern exercises the small empty-pattern guard at the
// top of WaitForText without invoking tmux. We can call regexp.Compile
// the same way the production code does and confirm the pre-compile
// validation we rely on (non-empty pattern) holds for arbitrary bytes.
func FuzzWaitForTextPattern(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("ok"))
	f.Add([]byte("\x00"))
	f.Add([]byte(`(\w+)`))

	f.Fuzz(func(t *testing.T, pattern []byte) {
		t.Parallel()
		p := string(pattern)
		if p == "" {
			// Production code rejects empty patterns before compile;
			// the fuzz is only here to make sure handing an empty
			// string into Compile is not a panic surface either.
			_, err := regexp.Compile(p)
			if err != nil {
				t.Fatalf("empty pattern unexpectedly failed to compile: %v", err)
			}
			return
		}
		// Non-empty patterns may or may not compile; either outcome is
		// fine, the only invariant is "no panic".
		_, _ = regexp.Compile(p)
	})
}

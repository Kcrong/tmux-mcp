package snapshot

import (
	"strings"
	"testing"
)

// FuzzRecordAndDiff exercises the public Store API with arbitrary byte
// sequences as old/new pane bodies. The store must:
//   - never panic on adversarial input,
//   - hand back a deterministic token for identical bodies,
//   - report Changed=false and zero diffs when consecutive bodies match,
//   - keep every reported diff line index inside the new body's line count,
//   - survive a bidirectional X→Y, Y→X round-trip without panicking.
func FuzzRecordAndDiff(f *testing.F) {
	f.Add([]byte(""), []byte(""))
	f.Add([]byte("alpha\nbeta\ngamma"), []byte("alpha\nBETA\ngamma"))
	f.Add([]byte("a\nb\nc"), []byte("a"))
	f.Add([]byte("one"), []byte("one\ntwo\nthree"))
	f.Add([]byte("\x00\n\xff"), []byte("\xff\n\x00"))
	f.Add([]byte("line1\rline2"), []byte("line1\nline2"))

	f.Fuzz(func(t *testing.T, oldBody, newBody []byte) {
		s := New()
		const sess = "fuzz"

		// First record establishes a baseline token.
		first := s.Record(sess, string(oldBody))
		if first.Token == "" {
			t.Fatalf("Record: empty token for body %q", oldBody)
		}
		if !first.Changed {
			t.Fatalf("Record: first call must report Changed=true, body=%q", oldBody)
		}

		// Identical Record again: same token, Changed=false, no diff.
		repeat := s.Record(sess, string(oldBody))
		if repeat.Token != first.Token {
			t.Fatalf("identical body produced different tokens: %q vs %q", first.Token, repeat.Token)
		}
		if repeat.Changed {
			t.Fatalf("identical body reported Changed=true, body=%q", oldBody)
		}

		// Tokens must be deterministic across stores.
		other := New()
		mirror := other.Record(sess, string(oldBody))
		if mirror.Token != first.Token {
			t.Fatalf("token not deterministic across stores: %q vs %q", first.Token, mirror.Token)
		}

		// DiffSince with the same body should yield no diffs.
		snapSame, diffsSame := s.DiffSince(sess, repeat.Token, string(oldBody))
		if snapSame.Changed {
			t.Fatalf("DiffSince(identical): Changed=true, body=%q", oldBody)
		}
		if len(diffsSame) != 0 {
			t.Fatalf("DiffSince(identical): expected no diffs, got %d", len(diffsSame))
		}

		// Forward diff: old -> new.
		snapFwd, diffsFwd := s.DiffSince(sess, snapSame.Token, string(newBody))
		assertDiffsInBounds(t, "forward", string(newBody), string(oldBody), diffsFwd)
		if string(oldBody) == string(newBody) && len(diffsFwd) != 0 {
			t.Fatalf("forward diff: equal bodies produced %d diffs", len(diffsFwd))
		}
		if string(oldBody) == string(newBody) && snapFwd.Changed {
			t.Fatalf("forward diff: equal bodies reported Changed=true")
		}

		// Reverse diff: new -> old, on a fresh store to keep history clean.
		s2 := New()
		base := s2.Record(sess, string(newBody))
		_, diffsRev := s2.DiffSince(sess, base.Token, string(oldBody))
		assertDiffsInBounds(t, "reverse", string(oldBody), string(newBody), diffsRev)

		// Stale-token path: token we never recorded must produce a full
		// reset listing every line in the new body and no panics.
		s3 := New()
		_ = s3.Record(sess, string(oldBody))
		_, diffsReset := s3.DiffSince(sess, "deadbeefdeadbeef", string(newBody))
		expectedLines := strings.Count(string(newBody), "\n") + 1
		if len(diffsReset) != expectedLines {
			t.Fatalf("reset diff: expected %d lines, got %d", expectedLines, len(diffsReset))
		}
		for i, d := range diffsReset {
			if d.Line != i {
				t.Fatalf("reset diff: index %d has Line=%d", i, d.Line)
			}
			if d.Old != "" {
				t.Fatalf("reset diff: line %d has non-empty Old=%q", i, d.Old)
			}
		}
	})
}

// FuzzLineDiff drives the internal lineDiff helper directly so we get
// coverage of the per-line path independent of the Store wrapper.
func FuzzLineDiff(f *testing.F) {
	f.Add([]byte(""), []byte(""))
	f.Add([]byte("a\nb"), []byte("a\nb"))
	f.Add([]byte("a\nb\nc"), []byte("a"))
	f.Add([]byte("a"), []byte("a\nb\nc"))
	f.Add([]byte("\n\n\n"), []byte(""))

	f.Fuzz(func(t *testing.T, oldBody, newBody []byte) {
		oldStr, newStr := string(oldBody), string(newBody)
		diffs := lineDiff(oldStr, newStr)

		newCount := strings.Count(newStr, "\n") + 1
		oldCount := strings.Count(oldStr, "\n") + 1
		maxLen := oldCount
		if newCount > maxLen {
			maxLen = newCount
		}
		for _, d := range diffs {
			if d.Line < 0 || d.Line >= maxLen {
				t.Fatalf("diff line %d out of bounds [0,%d) for old=%q new=%q", d.Line, maxLen, oldStr, newStr)
			}
			if d.Removed && d.New != "" {
				t.Fatalf("Removed=true must imply empty New, got %+v", d)
			}
		}

		// Equal inputs ⇒ no diffs.
		if oldStr == newStr && len(diffs) != 0 {
			t.Fatalf("equal bodies produced %d diffs", len(diffs))
		}

		// Symmetry sanity: the round-trip must also not panic, and an
		// X→Y diff being empty implies Y→X is empty too.
		rev := lineDiff(newStr, oldStr)
		if len(diffs) == 0 && len(rev) != 0 {
			t.Fatalf("X→Y empty but Y→X has %d diffs (old=%q new=%q)", len(rev), oldStr, newStr)
		}
	})
}

func assertDiffsInBounds(t *testing.T, label, newBody, oldBody string, diffs []Diff) {
	t.Helper()
	newCount := strings.Count(newBody, "\n") + 1
	oldCount := strings.Count(oldBody, "\n") + 1
	maxLen := oldCount
	if newCount > maxLen {
		maxLen = newCount
	}
	for _, d := range diffs {
		if d.Line < 0 || d.Line >= maxLen {
			t.Fatalf("%s diff: line %d out of bounds [0,%d)", label, d.Line, maxLen)
		}
	}
}

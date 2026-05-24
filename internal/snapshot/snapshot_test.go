package snapshot

import (
	"strings"
	"testing"
)

func TestRecord_DetectsChange(t *testing.T) {
	s := New()
	a := s.Record("sess", "hello")
	if a.Token == "" {
		t.Fatal("token empty")
	}
	if !a.Changed {
		t.Fatal("first record should report Changed=true")
	}
	b := s.Record("sess", "hello")
	if b.Token != a.Token {
		t.Fatalf("identical body should yield identical token, got %q vs %q", b.Token, a.Token)
	}
	if b.Changed {
		t.Fatal("identical body should report Changed=false")
	}
	c := s.Record("sess", "world")
	if c.Token == a.Token {
		t.Fatal("different body should yield different token")
	}
	if !c.Changed {
		t.Fatal("different body should report Changed=true")
	}
}

func TestDiffSince_ReturnsLineDiff(t *testing.T) {
	s := New()
	first := s.Record("sess", "alpha\nbeta\ngamma")
	if first.Token == "" {
		t.Fatal("token empty")
	}
	snap, diffs := s.DiffSince("sess", first.Token, "alpha\nBETA\ngamma")
	if !snap.Changed {
		t.Fatal("expected Changed=true")
	}
	if len(diffs) != 1 || diffs[0].Line != 1 || diffs[0].Old != "beta" || diffs[0].New != "BETA" {
		t.Fatalf("unexpected diff: %+v", diffs)
	}
}

func TestDiffSince_NoDiffWhenIdentical(t *testing.T) {
	s := New()
	first := s.Record("sess", "x\ny")
	snap, diffs := s.DiffSince("sess", first.Token, "x\ny")
	if snap.Changed {
		t.Fatal("expected Changed=false for identical body")
	}
	if len(diffs) != 0 {
		t.Fatalf("expected no diff, got %d", len(diffs))
	}
}

func TestDiffSince_FullResetWhenTokenUnknown(t *testing.T) {
	s := New()
	_ = s.Record("sess", "ignored")
	body := "one\ntwo\nthree"
	snap, diffs := s.DiffSince("sess", "deadbeef", body)
	if snap.Token == "" {
		t.Fatal("token empty")
	}
	if len(diffs) != 3 {
		t.Fatalf("expected 3 added lines, got %d", len(diffs))
	}
	for i, d := range diffs {
		if d.Line != i || d.Old != "" || d.New == "" {
			t.Fatalf("line %d: unexpected diff %+v", i, d)
		}
	}
}

func TestDiffSince_HandlesRemovedLines(t *testing.T) {
	s := New()
	first := s.Record("sess", "a\nb\nc")
	_, diffs := s.DiffSince("sess", first.Token, "a")
	// Lines 1 and 2 should be reported as removed.
	if len(diffs) != 2 {
		t.Fatalf("expected 2 removed lines, got %d (%+v)", len(diffs), diffs)
	}
	for _, d := range diffs {
		if !d.Removed {
			t.Errorf("line %d should be Removed: %+v", d.Line, d)
		}
	}
}

func TestForget_DropsSessionHistory(t *testing.T) {
	s := New()
	first := s.Record("sess", "alpha\nbeta")
	if first.Token == "" {
		t.Fatal("token empty")
	}
	if !s.Has("sess") {
		t.Fatal("Has should report true after Record")
	}

	// Sanity: prior token resolves before Forget.
	if _, diffs := s.DiffSince("sess", first.Token, "alpha\nBETA"); len(diffs) != 1 {
		t.Fatalf("pre-forget DiffSince should match prior token, got %d diffs", len(diffs))
	}

	// Forget dropped the entry, so the next access starts fresh.
	s.Forget("sess")
	if s.Has("sess") {
		t.Fatal("Forget did not remove entry")
	}

	// Reusing the old token now yields a full reset because history is gone.
	body := "one\ntwo"
	snap, diffs := s.DiffSince("sess", first.Token, body)
	if !snap.Changed {
		t.Fatal("DiffSince after Forget should report Changed=true")
	}
	if len(diffs) != 2 {
		t.Fatalf("expected full reset (2 added lines) after Forget, got %d diffs", len(diffs))
	}
	for i, d := range diffs {
		if d.Line != i || d.Old != "" || d.New == "" {
			t.Fatalf("line %d: expected added-line diff, got %+v", i, d)
		}
	}

	// Forgetting an unknown session is a no-op (must not panic).
	s.Forget("never-existed")
}

func TestDiffSince_PreservesPriorOnlyForOneStep(t *testing.T) {
	// History keeps two entries — the snapshot you just took and the one
	// before it. After three captures, the original token must be too
	// stale to diff against.
	s := New()
	first := s.Record("sess", "v1")
	_ = s.Record("sess", "v2")
	_ = s.Record("sess", "v3")
	_, diffs := s.DiffSince("sess", first.Token, "v4")
	// We don't have v1 anymore; expect a full reset.
	if !strings.Contains(diffs[0].New, "v4") {
		t.Fatalf("expected reset diff containing v4, got %+v", diffs)
	}
}

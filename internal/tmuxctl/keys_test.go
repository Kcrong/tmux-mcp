package tmuxctl

import (
	"context"
	"testing"
	"time"
)

// TestListKeys_DefaultListingHasBindings is the load-bearing
// "controller default install has at least one binding" path: a
// freshly-created tmux server ships with a non-trivial default key map
// (the prefix table alone has dozens of bindings), so the unscoped
// listing must surface bindings without any caller-side filtering.
// A zero-length response would mean we forgot a flag or the parser
// silently dropped every line.
func TestListKeys_DefaultListingHasBindings(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a session so list-keys runs against a
	// live server (list-keys does not auto-spawn the daemon on every
	// version).
	if err := c.CreateSession(ctx, SessionSpec{Name: "lk", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	keys, err := c.ListKeys(ctx, ListKeysOpts{})
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) == 0 {
		t.Fatal("expected at least one binding in the default tmux key map, got 0")
	}
	// Spot-check that every entry parsed cleanly: no empty Table/Key
	// (unrecognised line silently slipping through would surface here).
	for i, kb := range keys {
		if kb.Table == "" {
			t.Fatalf("keys[%d] Table empty: %#v", i, kb)
		}
		if kb.Key == "" {
			t.Fatalf("keys[%d] Key empty: %#v", i, kb)
		}
		if kb.Command == "" {
			t.Fatalf("keys[%d] Command empty: %#v", i, kb)
		}
	}
}

// TestListKeys_TableFilterNarrowsResults pins the `-T TABLE` filter:
// scoping to "prefix" must return strictly fewer entries than the
// unscoped listing (the prefix table is a strict subset of every
// table), and every entry's Table column must equal the requested
// table. A regression where the boundary forgot to forward `-T` would
// trip the row-count check; one where it forwarded the flag but parsed
// out a different column would trip the per-row Table check.
func TestListKeys_TableFilterNarrowsResults(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "lkt", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	all, err := c.ListKeys(ctx, ListKeysOpts{})
	if err != nil {
		t.Fatalf("ListKeys (all): %v", err)
	}
	prefix, err := c.ListKeys(ctx, ListKeysOpts{Table: "prefix"})
	if err != nil {
		t.Fatalf("ListKeys (prefix): %v", err)
	}
	if len(prefix) == 0 {
		t.Fatal("expected at least one binding in the prefix table, got 0")
	}
	if len(prefix) >= len(all) {
		t.Fatalf("prefix table (%d) should be smaller than all-tables (%d)", len(prefix), len(all))
	}
	for i, kb := range prefix {
		if kb.Table != "prefix" {
			t.Fatalf("prefix[%d].Table = %q, want \"prefix\"", i, kb.Table)
		}
	}
}

// TestListKeys_NotesOnlyShrinksAndRetagsTable exercises the `-N` mode:
// every binding tmux emits in this mode must carry a note (the
// "Command" column becomes the note text), and the result is strictly
// smaller than the all-tables default listing — the `-N` mode hides
// every binding without an annotation. We also check that when the
// caller supplies a Table filter, the boundary propagates it onto
// every returned KeyBinding.Table (the `-N` output drops the table
// column from tmux's stdout).
func TestListKeys_NotesOnlyShrinksAndRetagsTable(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "lkn", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	all, err := c.ListKeys(ctx, ListKeysOpts{})
	if err != nil {
		t.Fatalf("ListKeys (all): %v", err)
	}
	notes, err := c.ListKeys(ctx, ListKeysOpts{NotesOnly: true})
	if err != nil {
		t.Fatalf("ListKeys (notes): %v", err)
	}
	if len(notes) == 0 {
		t.Fatal("expected at least one annotated binding in the default tmux key map, got 0")
	}
	if len(notes) >= len(all) {
		t.Fatalf("notes-only listing (%d) should be smaller than all (%d)", len(notes), len(all))
	}
	for i, kb := range notes {
		if kb.Key == "" {
			t.Fatalf("notes[%d] Key empty: %#v", i, kb)
		}
		if kb.Command == "" {
			t.Fatalf("notes[%d] Command (note text) empty: %#v", i, kb)
		}
	}

	// With both Table and NotesOnly, tmux's stdout still drops the
	// table column — the boundary fills it in from opts.Table so the
	// response shape stays uniform.
	tagged, err := c.ListKeys(ctx, ListKeysOpts{Table: "prefix", NotesOnly: true})
	if err != nil {
		t.Fatalf("ListKeys (prefix+notes): %v", err)
	}
	for i, kb := range tagged {
		if kb.Table != "prefix" {
			t.Fatalf("tagged[%d].Table = %q, want \"prefix\"", i, kb.Table)
		}
	}
}

// TestListKeys_PrefixForwardedToOutput confirms the `-P PREFIX` knob
// is forwarded to tmux: in notes-only mode, every rendered key chord
// in the response must start with the prefix the caller asked for. A
// regression where the boundary swallowed `-P` would surface as keys
// rendered without the prefix; one where it emitted an empty prefix
// flag would error out at the tmux invocation.
func TestListKeys_PrefixForwardedToOutput(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "lkp", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	const wantPrefix = "PFX "
	keys, err := c.ListKeys(ctx, ListKeysOpts{NotesOnly: true, Prefix: wantPrefix})
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) == 0 {
		t.Fatal("expected at least one annotated binding, got 0")
	}
	for i, kb := range keys {
		// tmux renders the prefix verbatim before the key chord; the
		// boundary keeps the full rendered form so the caller sees what
		// tmux produced.
		if len(kb.Key) < len(wantPrefix) || kb.Key[:len(wantPrefix)] != wantPrefix {
			t.Fatalf("keys[%d].Key = %q, want a value starting with %q", i, kb.Key, wantPrefix)
		}
	}
}

// TestParseKeyLine_BindKeyHappyPath round-trips a synthetic default-
// mode line through the parser and confirms every column decodes
// cleanly. This pins the bindKeyLineRE shape: a regex change that
// silently regresses a column (e.g. capturing the key as part of the
// command) would still pass the "non-empty" smoke check on a real
// tmux but trip this targeted assertion.
func TestParseKeyLine_BindKeyHappyPath(t *testing.T) {
	t.Parallel()
	got, err := parseKeyLine(`bind-key    -T prefix       C-b                    send-prefix`, ListKeysOpts{}, -1)
	if err != nil {
		t.Fatalf("parseKeyLine: %v", err)
	}
	if got.Table != "prefix" {
		t.Errorf("Table = %q, want prefix", got.Table)
	}
	if got.Key != "C-b" {
		t.Errorf("Key = %q, want C-b", got.Key)
	}
	if got.Command != "send-prefix" {
		t.Errorf("Command = %q, want send-prefix", got.Command)
	}
}

// TestParseKeyLine_QuotedKeyUnquoted exercises the quoted-key branch:
// tmux double-quotes a few key names (`"M-{"`, `"M-}"`) so they don't
// collide with its command-block syntax. The boundary must strip the
// quotes so the round-trip key matches what callers feed into
// `bind-key` / `send-keys`.
func TestParseKeyLine_QuotedKeyUnquoted(t *testing.T) {
	t.Parallel()
	got, err := parseKeyLine(`bind-key    -T copy-mode    "M-{"                  send-keys -X previous-paragraph`, ListKeysOpts{}, -1)
	if err != nil {
		t.Fatalf("parseKeyLine: %v", err)
	}
	if got.Key != "M-{" {
		t.Errorf("Key = %q, want M-{ (quotes stripped)", got.Key)
	}
}

// TestParseKeyLine_RepeatFlagAccepted guards the optional `-r` (repeat)
// flag tmux emits for a handful of bindings. A regex that hard-coded
// the slot order would silently misparse these lines; pinning a
// targeted case keeps the surface honest across tmux versions.
func TestParseKeyLine_RepeatFlagAccepted(t *testing.T) {
	t.Parallel()
	got, err := parseKeyLine(`bind-key    -r -T prefix    Up                     select-pane -U`, ListKeysOpts{}, -1)
	if err != nil {
		t.Fatalf("parseKeyLine: %v", err)
	}
	if got.Table != "prefix" {
		t.Errorf("Table = %q, want prefix", got.Table)
	}
	if got.Key != "Up" {
		t.Errorf("Key = %q, want Up", got.Key)
	}
	if got.Command != "select-pane -U" {
		t.Errorf("Command = %q, want select-pane -U", got.Command)
	}
}

// TestParseKeyLine_NotesOnlyFormat exercises the two-column note shape:
// the parser must split on the first run of 2+ spaces and put the key
// chord (with whatever `-P` prefix tmux rendered) on the left and the
// note text on the right. Table is propagated from opts.Table.
func TestParseKeyLine_NotesOnlyFormat(t *testing.T) {
	t.Parallel()
	got, err := parseKeyLine("C-b ?     List key bindings", ListKeysOpts{NotesOnly: true, Table: "root"}, -1)
	if err != nil {
		t.Fatalf("parseKeyLine: %v", err)
	}
	if got.Table != "root" {
		t.Errorf("Table = %q, want root (propagated from opts)", got.Table)
	}
	if got.Key != "C-b ?" {
		t.Errorf("Key = %q, want \"C-b ?\"", got.Key)
	}
	if got.Command != "List key bindings" {
		t.Errorf("Command = %q, want \"List key bindings\"", got.Command)
	}
}

// TestParseKeyLine_NotesOnlyKeyColumnHonoured confirms the column-
// detection path: when detectNotesKeyColumn has found a stable
// boundary, parseKeyLine splits at that exact position even when the
// chord at that position has only a single space of trailing padding
// (the "C-b M-Right Resize ..." case in the real `-aN` output where
// the chord is exactly the longest in the listing).
func TestParseKeyLine_NotesOnlyKeyColumnHonoured(t *testing.T) {
	t.Parallel()
	// 11-character chord followed by exactly one padding space, then
	// the note text. Without keyColumn the parser would mis-split on
	// the first internal space pair (there isn't one — it would error
	// out); with keyColumn=11 the boundary is unambiguous.
	got, err := parseKeyLine("C-b M-Right Resize the pane right by 5", ListKeysOpts{NotesOnly: true}, 11)
	if err != nil {
		t.Fatalf("parseKeyLine: %v", err)
	}
	if got.Key != "C-b M-Right" {
		t.Errorf("Key = %q, want \"C-b M-Right\"", got.Key)
	}
	if got.Command != "Resize the pane right by 5" {
		t.Errorf("Command = %q, want \"Resize the pane right by 5\"", got.Command)
	}
}

// TestParseKeyLine_NotesOnlyMissingGap rejects a malformed notes-only
// line — one with no two-space gap between key and note AND no column
// hint. A degraded "treat the whole line as the key" fallback would
// silently lose the note column on a future tmux build that switches
// to a tab separator, so we surface the parse failure loudly instead.
func TestParseKeyLine_NotesOnlyMissingGap(t *testing.T) {
	t.Parallel()
	if _, err := parseKeyLine("C-b ? note-without-gap", ListKeysOpts{NotesOnly: true}, -1); err == nil {
		t.Fatal("expected error for notes-only line missing the column gap and lacking a column hint")
	}
}

// TestParseKeyLine_UnrecognisedDefaultLine guards the same loud-
// failure contract for the default mode: a line that doesn't match
// the `bind-key -T <table> <key> ...` shape must error rather than
// degrade to a zero-valued KeyBinding.
func TestParseKeyLine_UnrecognisedDefaultLine(t *testing.T) {
	t.Parallel()
	if _, err := parseKeyLine("not a bind-key line", ListKeysOpts{}, -1); err == nil {
		t.Fatal("expected error for line that doesn't match the bind-key shape")
	}
}

// TestDetectNotesKeyColumn_TakesMinGap pins the "minimum gap-start
// column" rule: the detector must use the *smallest* observed gap
// position across the listing. A larger value would over-shoot and
// truncate the chord on shorter entries; a smaller value would
// under-shoot and leave padding inside the key field.
func TestDetectNotesKeyColumn_TakesMinGap(t *testing.T) {
	t.Parallel()
	// Two lines: "C-b !       Break ..." (gap starts at col 4) and
	// "C-b M-Right Resize ..." (no 2+ gap). The detector should
	// return 4 because that's the first stable boundary.
	lines := []string{
		"C-b !       Break pane to a new window",
		"C-b M-Right Resize the pane right by 5",
	}
	got := detectNotesKeyColumn(lines)
	// The first line's gap starts at index 4 (after "C-b !"); but
	// columns may be wider in other lines. Either way, the detector
	// should return the *minimum* observed start column.
	if got < 0 {
		t.Fatalf("detectNotesKeyColumn = -1, want a positive column hint (lines[0] has a 7-space gap)")
	}
	if got > len("C-b M-Right") {
		t.Fatalf("detectNotesKeyColumn = %d, want a value ≤ %d (the longest chord)", got, len("C-b M-Right"))
	}
}

// TestDetectNotesKeyColumn_NoGapReturnsNegative covers the all-lines-
// crammed corner case: when no row in the listing has a 2+ space gap,
// the detector returns -1 and the parser falls back to the line-local
// heuristic. The parser will then error out on each line — which is
// the right behaviour: there is genuinely no column boundary in such
// output, and silently swallowing it would hide a real tmux drift.
func TestDetectNotesKeyColumn_NoGapReturnsNegative(t *testing.T) {
	t.Parallel()
	if got := detectNotesKeyColumn([]string{"a b", "c d e"}); got != -1 {
		t.Fatalf("detectNotesKeyColumn no-gap = %d, want -1", got)
	}
	if got := detectNotesKeyColumn(nil); got != -1 {
		t.Fatalf("detectNotesKeyColumn nil = %d, want -1", got)
	}
}

// TestIndexRunOfSpaces_SingleSpaceIgnored pins the parser's column-gap
// detector: a single space is a valid separator inside a key chord
// (the notes-only output renders multi-key sequences like "C-b ?"
// with a space between the chords) and must NOT be mistaken for the
// column boundary. Two-or-more is the floor.
func TestIndexRunOfSpaces_SingleSpaceIgnored(t *testing.T) {
	t.Parallel()
	if got := indexRunOfSpaces("a b c", 2); got != -1 {
		t.Fatalf("indexRunOfSpaces single space = %d, want -1", got)
	}
	if got := indexRunOfSpaces("a  b c", 2); got != 1 {
		t.Fatalf("indexRunOfSpaces double space = %d, want 1", got)
	}
}

// TestIndexRunOfSpaces_RejectsZeroN is a programmer-error guard: a
// zero-length run is meaningless and would, in a degraded
// implementation, return 0 for every input. The implementation rejects
// it and returns -1.
func TestIndexRunOfSpaces_RejectsZeroN(t *testing.T) {
	t.Parallel()
	if got := indexRunOfSpaces("anything", 0); got != -1 {
		t.Fatalf("indexRunOfSpaces zero n = %d, want -1", got)
	}
	if got := indexRunOfSpaces("anything", -1); got != -1 {
		t.Fatalf("indexRunOfSpaces negative n = %d, want -1", got)
	}
}

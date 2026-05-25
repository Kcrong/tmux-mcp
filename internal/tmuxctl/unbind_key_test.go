package tmuxctl

import (
	"context"
	"strings"
	"testing"
	"time"
)

// keyExistsInTable reports whether a chord/table pair is currently
// bound on the controller's tmux server. It drives ListKeys with the
// table filter and walks the response — the canonical way to probe a
// binding's presence at the boundary level (no `tmux list-keys -F`
// helper exists across the supported version range, and this helper
// keeps every UnbindKey test using the same observation surface).
//
// The "table doesn't exist" stderr from list-keys (which tmux emits
// when the targeted custom table has no live bindings — e.g. after
// the last binding in it was removed) is treated as "no bindings"
// here, since at the unbind contract level the table-gone shape is
// indistinguishable from "the chord is not bound".
func keyExistsInTable(t *testing.T, c *Controller, ctx context.Context, table, key string) bool {
	t.Helper()
	keys, err := c.ListKeys(ctx, ListKeysOpts{Table: table})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "doesn't exist") {
			return false
		}
		t.Fatalf("ListKeys(%q): %v", table, err)
	}
	for _, kb := range keys {
		if kb.Key == key {
			return true
		}
	}
	return false
}

// bindKey calls `tmux bind-key -T TABLE KEY <command>` directly via the
// controller's run() pipe so the UnbindKey suite has a way to populate a
// fresh binding without depending on the not-yet-merged BindKey
// boundary. The bind targets a no-op so the test never has to reason
// about side effects when (or if) the chord ever fires.
func bindKey(t *testing.T, c *Controller, ctx context.Context, table, key string) {
	t.Helper()
	if _, err := c.run(ctx, "bind-key", "-T", table, key, "display-message", "noop"); err != nil {
		t.Fatalf("bind-key %s/%s: %v", table, key, err)
	}
}

// TestUnbindKey_SingleByTable is the load-bearing happy path: bind a
// fresh chord in a custom table, observe it appears in list-keys,
// UnbindKey it, observe it is gone. A regression where the boundary
// dropped `-T TABLE` from the unbind call would either no-op (the
// binding is in a table we did not target) or wipe the wrong table —
// both surface as the post-unbind ListKeys still showing the chord.
func TestUnbindKey_SingleByTable(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the daemon. Some tmux versions refuse list-keys / bind-key
	// before the server has at least one client/session.
	if err := c.CreateSession(ctx, SessionSpec{Name: "ub", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	const table = "ub-test"
	const key = "F12"

	bindKey(t, c, ctx, table, key)
	if !keyExistsInTable(t, c, ctx, table, key) {
		t.Fatalf("pre-condition: %s/%s not present after bind-key", table, key)
	}

	if err := c.UnbindKey(ctx, key, table, false); err != nil {
		t.Fatalf("UnbindKey: %v", err)
	}
	if keyExistsInTable(t, c, ctx, table, key) {
		t.Fatalf("post-condition: %s/%s still present after UnbindKey", table, key)
	}
}

// TestUnbindKey_AllInTable exercises the `-a` path: bind two distinct
// keys in the same table, ask UnbindKey to wipe the whole table, and
// confirm both vanish. The custom-table scope keeps the test from
// disturbing tmux's built-in tables (which would surface as flaky
// neighbour tests) and proves `-a` is forwarded onto argv.
func TestUnbindKey_AllInTable(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "uba", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	const table = "ub-all"
	bindKey(t, c, ctx, table, "F11")
	bindKey(t, c, ctx, table, "F10")
	for _, k := range []string{"F11", "F10"} {
		if !keyExistsInTable(t, c, ctx, table, k) {
			t.Fatalf("pre-condition: %s/%s not present after bind-key", table, k)
		}
	}

	if err := c.UnbindKey(ctx, "", table, true); err != nil {
		t.Fatalf("UnbindKey(-a): %v", err)
	}
	for _, k := range []string{"F11", "F10"} {
		if keyExistsInTable(t, c, ctx, table, k) {
			t.Fatalf("post-condition: %s/%s still present after UnbindKey -a", table, k)
		}
	}
}

// TestUnbindKey_DoubleUnbindIsIdempotent pins the idempotency contract
// end-to-end: a second UnbindKey on a chord that is no longer bound
// must succeed silently. Tmux's `unbind-key` is itself idempotent on
// every supported version, but a regression in the boundary's error
// mapping (e.g. one that started treating empty stderr as an error)
// would surface here exactly as the "agent re-runs its setup script"
// path hits in production.
func TestUnbindKey_DoubleUnbindIsIdempotent(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "ubi", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	const table = "ub-idem"
	const key = "F9"

	bindKey(t, c, ctx, table, key)
	if err := c.UnbindKey(ctx, key, table, false); err != nil {
		t.Fatalf("UnbindKey first call: %v", err)
	}
	// Second call against a now-unbound chord must still succeed.
	if err := c.UnbindKey(ctx, key, table, false); err != nil {
		t.Fatalf("UnbindKey second call (idempotency): %v", err)
	}
}

// TestUnbindKey_RejectsBothEmpty pins the "neither key nor -a" guard:
// a call with key="" and all=false would silently no-op on tmux and is
// almost certainly a programmer error, so the boundary refuses it up
// front. Without this guard a buggy caller would see a successful
// no-op and wonder why their unbind never landed.
func TestUnbindKey_RejectsBothEmpty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.UnbindKey(ctx, "", "", false)
	if err == nil {
		t.Fatal("expected error for empty key + all=false (silently no-op shape)")
	}
	if !strings.Contains(err.Error(), "must set either key or all=true") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestUnbindKey_RejectsBothSet pins the inverse contradiction: `-a`
// (wipe everything) plus an explicit KEY do not have a single
// well-defined meaning, and tmux's behaviour across versions is
// "swallow KEY silently". Refusing the shape here keeps the boundary
// from inheriting that footgun.
func TestUnbindKey_RejectsBothSet(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.UnbindKey(ctx, "C-a", "prefix", true)
	if err == nil {
		t.Fatal("expected error for all=true + non-empty key (tmux ignores the key silently)")
	}
	if !strings.Contains(err.Error(), "cannot be combined with a non-empty key") {
		t.Fatalf("unexpected error: %v", err)
	}
}

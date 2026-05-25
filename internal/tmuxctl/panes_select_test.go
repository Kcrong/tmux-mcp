package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSelectPaneAdvanced_Succeeds drives the happy path: split a fresh
// session into two panes, walk to a directional neighbour with
// SelectPaneAdvanced(direction="down"), and confirm the active pane
// actually flipped. ListPanes is the only observation channel that
// doesn't reach back into tmux's interactive state, so we use it to pin
// which pane carries the active flag after the call.
func TestSelectPaneAdvanced_Succeeds(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "spa", Command: "/bin/sh", Width: 80, Height: 40}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.SplitPane(ctx, SplitOptions{
		Session: "spa", Direction: "vertical", Detach: true,
	}); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}

	// Start by selecting the top pane explicitly so the directional walk
	// has a deterministic origin.
	if err := c.SelectPaneAdvanced(ctx, SelectPaneOptions{Target: "spa:0.0"}); err != nil {
		t.Fatalf("SelectPaneAdvanced(top): %v", err)
	}
	if err := c.SelectPaneAdvanced(ctx, SelectPaneOptions{
		Target: "spa:0.0", Direction: "down",
	}); err != nil {
		t.Fatalf("SelectPaneAdvanced(down): %v", err)
	}

	panes, err := c.ListPanes(ctx, "spa")
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) < 2 {
		t.Fatalf("expected 2 panes after split, got %d", len(panes))
	}
	var active *Pane
	for i := range panes {
		if panes[i].Active {
			active = &panes[i]
			break
		}
	}
	if active == nil {
		t.Fatal("no pane reported as active after SelectPaneAdvanced")
	}
	if active.Index == 0 {
		t.Fatalf("expected the down-neighbour to be active, got index=%d", active.Index)
	}
}

// TestSelectPaneAdvanced_ZoomFlag exercises the -Z code path. tmux's
// observable zoom side-effect varies across versions (3.4 only flips
// `window_zoomed_flag` when the call also moves focus to a different
// pane), so the assertion is the load-bearing one for the controller:
// the call returns no error and a directly-following ListPanes still
// shows two panes — i.e. the -Z flag did not corrupt the surrounding
// layout. The boundary's job is to forward the flag faithfully; the
// observable zoom behaviour is tmux's contract, not ours.
func TestSelectPaneAdvanced_ZoomFlag(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "spz", Command: "/bin/sh", Width: 80, Height: 40}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.SplitPane(ctx, SplitOptions{
		Session: "spz", Direction: "horizontal", Detach: true,
	}); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}

	if err := c.SelectPaneAdvanced(ctx, SelectPaneOptions{Target: "spz:0.0", Zoom: true}); err != nil {
		t.Fatalf("SelectPaneAdvanced(zoom): %v", err)
	}
	panes, err := c.ListPanes(ctx, "spz")
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("expected 2 panes after select-pane -Z, got %d", len(panes))
	}
}

// TestSelectPaneAdvanced_MarkAndUnmark drives the -m / -M path: marking
// a pane sets `pane_marked` to "1" tmux-side, and a follow-up call with
// Unmark flips it back to "0". DisplayMessage with the pane format
// variable is the observation channel — the cheapest way to read tmux's
// internal flag without spinning up the interactive client. (tmux only
// keeps a single marked pane server-wide, so the assertions are exact.)
func TestSelectPaneAdvanced_MarkAndUnmark(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "spm", Command: "/bin/sh", Width: 80, Height: 40}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.SelectPaneAdvanced(ctx, SelectPaneOptions{Target: "spm:0.0", Mark: true}); err != nil {
		t.Fatalf("SelectPaneAdvanced(mark): %v", err)
	}
	got, err := c.DisplayMessage(ctx, "#{pane_marked}", "spm", "", "")
	if err != nil {
		t.Fatalf("DisplayMessage(pane_marked): %v", err)
	}
	if strings.TrimSpace(got) != "1" {
		t.Fatalf("pane_marked after Mark=true = %q, want \"1\"", got)
	}

	if uerr := c.SelectPaneAdvanced(ctx, SelectPaneOptions{Target: "spm:0.0", Unmark: true}); uerr != nil {
		t.Fatalf("SelectPaneAdvanced(unmark): %v", uerr)
	}
	got, err = c.DisplayMessage(ctx, "#{pane_marked}", "spm", "", "")
	if err != nil {
		t.Fatalf("DisplayMessage(pane_marked) post-unmark: %v", err)
	}
	if strings.TrimSpace(got) != "0" {
		t.Fatalf("pane_marked after Unmark=true = %q, want \"0\"", got)
	}
}

// TestSelectPaneAdvanced_MissingSessionWrapsSentinel pins the typed
// sentinel for an unknown target — needed by the JSON-RPC layer to map
// to CodeSessionNotFound. tmux phrases this as "can't find pane" so the
// controller has to translate it back to ErrSessionNotFound.
func TestSelectPaneAdvanced_MissingSessionWrapsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err := c.SelectPaneAdvanced(ctx, SelectPaneOptions{Target: "ghost_session_nonexistent:0.0"})
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSelectPaneAdvanced_RejectsEmptyTarget locks down the up-front
// guard. tmux would otherwise act on whatever pane it considers current,
// which is rarely what the caller intended.
func TestSelectPaneAdvanced_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	c := &Controller{}
	err := c.SelectPaneAdvanced(context.Background(), SelectPaneOptions{})
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if !strings.Contains(err.Error(), "target required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSelectPaneAdvanced_RejectsConflictingFlags pins the mutual
// exclusion checks for mark/unmark and enable_input/disable_input.
// tmux would silently accept the contradictory pair (last flag wins);
// the boundary refuses up front so a buggy caller fails loudly.
func TestSelectPaneAdvanced_RejectsConflictingFlags(t *testing.T) {
	t.Parallel()
	c := &Controller{}
	tests := []struct {
		name    string
		opts    SelectPaneOptions
		wantSub string
	}{
		{
			name:    "mark+unmark",
			opts:    SelectPaneOptions{Target: "demo:0.0", Mark: true, Unmark: true},
			wantSub: "mark and unmark",
		},
		{
			name:    "enable+disable",
			opts:    SelectPaneOptions{Target: "demo:0.0", EnableInput: true, DisableInput: true},
			wantSub: "enable_input and disable_input",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.SelectPaneAdvanced(context.Background(), tc.opts)
			if err == nil {
				t.Fatal("expected error for conflicting flags")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestSelectPaneAdvanced_RejectsBadDirection guards the controller-side
// direction whitelist. tmux's own diagnostic for an unknown flag is
// terse; the controller's message names the offending value verbatim so
// the operator sees the typo immediately.
func TestSelectPaneAdvanced_RejectsBadDirection(t *testing.T) {
	t.Parallel()
	c := &Controller{}
	err := c.SelectPaneAdvanced(context.Background(), SelectPaneOptions{
		Target:    "demo:0.0",
		Direction: "diagonal",
	})
	if err == nil {
		t.Fatal("expected error for invalid direction")
	}
	if !strings.Contains(err.Error(), "diagonal") {
		t.Fatalf("error %q does not name the offending direction", err.Error())
	}
}

// TestSelectPaneDirectionFlag_RoundTrip is a cheap unit test that locks
// the four directional aliases against their tmux flags. Drift here
// would silently mis-route a directional select.
func TestSelectPaneDirectionFlag_RoundTrip(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"":      "",
		"up":    "-U",
		"down":  "-D",
		"left":  "-L",
		"right": "-R",
	}
	for in, expected := range want {
		got, err := selectPaneDirectionFlag(in)
		if err != nil {
			t.Errorf("selectPaneDirectionFlag(%q) err = %v", in, err)
			continue
		}
		if got != expected {
			t.Errorf("selectPaneDirectionFlag(%q) = %q, want %q", in, got, expected)
		}
	}
	if _, err := selectPaneDirectionFlag("nowhere"); err == nil {
		t.Error("expected error for unknown direction")
	}
}

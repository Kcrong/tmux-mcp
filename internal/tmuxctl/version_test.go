package tmuxctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

func TestParseTmuxVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		in        string
		wantMajor int
		wantMinor int
		wantDev   bool
		wantErr   bool
	}{
		{name: "stable", in: "tmux 3.4", wantMajor: 3, wantMinor: 4},
		{name: "letter suffix", in: "tmux 3.4a", wantMajor: 3, wantMinor: 4},
		{name: "with newline", in: "tmux 3.0\n", wantMajor: 3, wantMinor: 0},
		{name: "old 2.x", in: "tmux 2.6", wantMajor: 2, wantMinor: 6},
		{name: "very old 1.x", in: "tmux 1.8", wantMajor: 1, wantMinor: 8},
		{name: "next dev", in: "tmux next-3.5", wantMajor: 3, wantMinor: 5, wantDev: true},
		{name: "next dev with letter", in: "tmux next-3.5a", wantMajor: 3, wantMinor: 5, wantDev: true},
		{name: "master dev", in: "tmux master", wantDev: true},
		{name: "openbsd-portable banner", in: "tmux openbsd", wantDev: true},
		{name: "no banner", in: "3.4", wantMajor: 3, wantMinor: 4},
		{name: "empty", in: "", wantErr: true},
		{name: "blank", in: "   \n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, err := parseTmuxVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %+v", tc.in, v)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTmuxVersion(%q) error: %v", tc.in, err)
			}
			if v.major != tc.wantMajor || v.minor != tc.wantMinor {
				t.Fatalf("parseTmuxVersion(%q) = %d.%d, want %d.%d",
					tc.in, v.major, v.minor, tc.wantMajor, tc.wantMinor)
			}
			if v.dev != tc.wantDev {
				t.Fatalf("parseTmuxVersion(%q) dev = %v, want %v",
					tc.in, v.dev, tc.wantDev)
			}
		})
	}
}

func TestTmuxVersionAtLeast(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    tmuxVersion
		ok   bool
	}{
		{name: "exact 3.0", v: tmuxVersion{major: 3, minor: 0}, ok: true},
		{name: "newer 3.4", v: tmuxVersion{major: 3, minor: 4}, ok: true},
		{name: "newer 4.0", v: tmuxVersion{major: 4, minor: 0}, ok: true},
		{name: "older 2.6", v: tmuxVersion{major: 2, minor: 6}, ok: false},
		{name: "older 2.9", v: tmuxVersion{major: 2, minor: 9}, ok: false},
		{name: "much older 1.8", v: tmuxVersion{major: 1, minor: 8}, ok: false},
		{name: "dev passes", v: tmuxVersion{dev: true}, ok: true},
		{name: "next-3.5 marked dev", v: tmuxVersion{major: 3, minor: 5, dev: true}, ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.v.atLeast(minTmuxMajor, minTmuxMinor); got != tc.ok {
				t.Fatalf("%+v.atLeast(3,0) = %v, want %v", tc.v, got, tc.ok)
			}
		})
	}
}

// fakeTmux writes a tiny shell script that prints fixed output to stdout
// when invoked with -V, and returns its absolute path.
func fakeTmux(t *testing.T, version string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tmux script needs unix-like shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\nif [ \"$1\" = \"-V\" ]; then echo \"" + version + "\"; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	return path
}

func TestCheckTmuxVersion_TooOld(t *testing.T) {
	t.Parallel()
	bin := fakeTmux(t, "tmux 2.6")
	err := checkTmuxVersion(context.Background(), bin)
	if err == nil {
		t.Fatal("expected error for tmux 2.6")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tmux 3.0+ required") {
		t.Fatalf("missing required-version phrase: %q", msg)
	}
	if !strings.Contains(msg, "found 2.6") {
		t.Fatalf("missing found-version phrase: %q", msg)
	}
	if !strings.Contains(msg, "apt-get install tmux") ||
		!strings.Contains(msg, "brew upgrade tmux") {
		t.Fatalf("missing upgrade hint: %q", msg)
	}
	// Must wrap the typed sentinel so the JSON-RPC layer can map it to
	// CodeTmuxVersionUnsupported (-32001).
	if !errors.Is(err, errs.ErrTmuxVersionUnsupported) {
		t.Fatalf("error %v does not wrap errs.ErrTmuxVersionUnsupported", err)
	}
}

func TestCheckTmuxVersion_OK(t *testing.T) {
	t.Parallel()
	for _, ver := range []string{"tmux 3.0", "tmux 3.4a", "tmux next-3.5", "tmux master"} {
		t.Run(ver, func(t *testing.T) {
			t.Parallel()
			bin := fakeTmux(t, ver)
			if err := checkTmuxVersion(context.Background(), bin); err != nil {
				t.Fatalf("checkTmuxVersion(%q) = %v", ver, err)
			}
		})
	}
}

// TestNew_RealTmuxIfNewEnough is a tiny integration check for the New()
// path: when tmux is available locally we just make sure the version
// gate does not reject it.
func TestNew_RealTmuxIfNewEnough(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })
	if c.bin == "" || c.socket == "" {
		t.Fatalf("controller not initialised: %+v", c)
	}
}

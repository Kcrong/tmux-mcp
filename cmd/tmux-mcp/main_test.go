package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-version"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("run(-version): %v (stderr=%q)", err, stderr.String())
	}
	got := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(got, "tmux-mcp ") {
		t.Fatalf("expected version line, got %q", got)
	}
}

func TestHelpFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr)
	// flag.ContinueOnError surfaces -help as flag.ErrHelp; treat that as success.
	if err != nil && err.Error() != "flag: help requested" {
		t.Fatalf("run(-help): unexpected error %v", err)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("expected usage in stderr, got %q", stderr.String())
	}
}

func TestUnknownFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-nope"}, strings.NewReader(""), &stdout, &stderr); err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
}

func TestPositionalArgsRejected(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"oops"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for positional arg, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected positional argument") {
		t.Fatalf("expected positional-arg error, got %v", err)
	}
}

func TestInvalidLogLevelRejected(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-log-level=loud"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for invalid log level, got nil")
	}
	if !strings.Contains(err.Error(), "invalid -log-level") {
		t.Fatalf("expected invalid -log-level error, got %v", err)
	}
}

// TestRelativeSocketRejected makes sure the surface validation in
// tmuxctl.NewWithSocket bubbles up through main.run, so users see the
// error message instead of a confused "no server running" later.
func TestRelativeSocketRejected(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{"-socket=relative/sock"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for relative socket path")
	}
	if !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSocketEnvFallback covers the env-var path: when -socket is not
// passed but TMUX_MCP_SOCKET is set to a bogus relative value, run()
// must still surface the validation error rather than silently fall
// through to MkdirTemp.
func TestSocketEnvFallback(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	t.Setenv("TMUX_MCP_SOCKET", "relative/sock-from-env")
	var stdout, stderr bytes.Buffer
	err := run(nil, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for relative socket path from env")
	}
	if !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseLogLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"error", false},
		{"warn", false},
		{"warning", false},
		{"info", false},
		{"DEBUG", false},
		{"", false},
		{"trace", true},
	}
	for _, tc := range cases {
		_, err := parseLogLevel(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseLogLevel(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
		}
	}
}

// TestProbeFlag exercises the happy path of `tmux-mcp -probe` end-to-end
// through run(): with tmux on PATH the probe prints the
// "ok\ttmux=<v>\ttmux-mcp=<v>" line on stdout, writes nothing on
// stderr, and returns nil so the binary exits 0.
func TestProbeFlag(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-probe"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("run(-probe): %v (stderr=%q)", err, stderr.String())
	}
	got := stdout.String()
	if !strings.HasPrefix(got, "ok\ttmux=") {
		t.Fatalf("expected stdout to start with %q, got %q", "ok\ttmux=", got)
	}
	if !strings.Contains(got, "\ttmux-mcp=") {
		t.Fatalf("expected stdout to contain tmux-mcp version field, got %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("expected stdout to end with newline, got %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr on success, got %q", stderr.String())
	}
}

// TestRunProbeFailure unit-tests runProbe directly with a stripped-down
// PATH so tmux cannot be found. The function must write a "probe
// failed: " diagnostic to stderr, leave stdout untouched, and return an
// error that matches errProbeFailed so main() can suppress the slog
// duplicate.
func TestRunProbeFailure(t *testing.T) {
	t.Setenv("PATH", "/nonexistent-empty-dir-for-probe-test")
	var stdout, stderr bytes.Buffer
	err := runProbe(&stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when tmux is not on PATH")
	}
	if !errors.Is(err, errProbeFailed) {
		t.Fatalf("expected errProbeFailed, got %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout on failure, got %q", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), "probe failed: ") {
		t.Fatalf("expected stderr to start with %q, got %q",
			"probe failed: ", stderr.String())
	}
}

// TestRunProbeSuccess unit-tests runProbe directly with a real tmux on
// PATH so we can assert the exact tab-delimited shape and field order
// without spinning up a subprocess. Skips when tmux is unavailable.
func TestRunProbeSuccess(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	var stdout, stderr bytes.Buffer
	if err := runProbe(&stdout, &stderr); err != nil {
		t.Fatalf("runProbe: %v (stderr=%q)", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr on success, got %q", stderr.String())
	}
	line := strings.TrimSuffix(stdout.String(), "\n")
	parts := strings.Split(line, "\t")
	if len(parts) != 3 {
		t.Fatalf("expected 3 tab-separated fields, got %d in %q", len(parts), line)
	}
	if parts[0] != "ok" {
		t.Fatalf("expected first field %q, got %q", "ok", parts[0])
	}
	if !strings.HasPrefix(parts[1], "tmux=") {
		t.Fatalf("expected second field to start with %q, got %q", "tmux=", parts[1])
	}
	if !strings.HasPrefix(parts[2], "tmux-mcp=") {
		t.Fatalf("expected third field to start with %q, got %q", "tmux-mcp=", parts[2])
	}
}

// TestDebugLevelEmitsJSONLogs is a smoke test: with -log-level=debug a
// malformed request line on stdin must produce a JSON-formatted slog
// record on stderr (and stdout must stay valid JSON-RPC).
func TestDebugLevelEmitsJSONLogs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// "not json\n" gets stdin EOF after one line, so Serve returns and
	// run() unwinds cleanly. The malformed line trips the "invalid
	// request" warn-level log, which is well above debug.
	err := run([]string{"-log-level=debug"}, strings.NewReader("not json\n"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v stderr=%q", err, stderr.String())
	}
	// stderr must contain at least one slog JSON record.
	gotLog := false
	for line := range strings.SplitSeq(stderr.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil {
			if _, ok := rec["level"]; ok {
				gotLog = true
				break
			}
		}
	}
	if !gotLog {
		t.Fatalf("expected JSON slog record on stderr, got %q", stderr.String())
	}
	// stdout should hold the JSON-RPC parse-error response and nothing else.
	body := strings.TrimSpace(stdout.String())
	if !strings.Contains(body, "-32700") {
		t.Fatalf("expected JSON-RPC parse error on stdout, got %q", body)
	}
}

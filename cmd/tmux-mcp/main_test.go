package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
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
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-nope"}, strings.NewReader(""), &stdout, &stderr); err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
}

func TestPositionalArgsRejected(t *testing.T) {
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
	var stdout, stderr bytes.Buffer
	err := run([]string{"-log-level=loud"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for invalid log level, got nil")
	}
	if !strings.Contains(err.Error(), "invalid -log-level") {
		t.Fatalf("expected invalid -log-level error, got %v", err)
	}
}

func TestParseLogLevel(t *testing.T) {
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

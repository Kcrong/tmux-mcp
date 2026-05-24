package main

import (
	"bytes"
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

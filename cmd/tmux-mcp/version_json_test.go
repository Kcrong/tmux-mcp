package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEmitVersionJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := emitVersionJSON(&buf, "v1.2.3", "go1.24.6"); err != nil {
		t.Fatalf("emitVersionJSON: %v", err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("expected trailing newline, got %q", out)
	}
	var got versionJSONInfo
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	if got.Version != "v1.2.3" {
		t.Errorf("version: got %q want %q", got.Version, "v1.2.3")
	}
	if got.Go != "go1.24.6" {
		t.Errorf("go: got %q want %q", got.Go, "go1.24.6")
	}
	// commit and date may be empty in `go test` runs (no -trimpath, no
	// vcs.* settings), so we only assert that the fields exist in the
	// emitted JSON — that's covered by the Unmarshal above.
}

func TestEmitVersionJSONFieldsPresent(t *testing.T) {
	// Decode into a generic map to confirm every advertised key is
	// present in the output even when its value is the empty string.
	var buf bytes.Buffer
	if err := emitVersionJSON(&buf, "dev", "go1.24.6"); err != nil {
		t.Fatalf("emitVersionJSON: %v", err)
	}
	var raw map[string]string
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	for _, key := range []string{"version", "go", "commit", "date"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing %q key in %q", key, buf.String())
		}
	}
}

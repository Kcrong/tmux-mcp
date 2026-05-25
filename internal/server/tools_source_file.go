package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// maxSourceFilePathLen caps the `path` argument source_file accepts.
// 4096 bytes matches the conventional Linux PATH_MAX so any realistic
// absolute config-file path fits, while a hostile or buggy caller
// cannot stuff multi-megabyte strings into argv before tmux ever runs.
const maxSourceFilePathLen = 4096

// sourceFileToolDefs holds the JSON Schema for the source_file tool. It
// is appended onto the main toolDefs slice from this file's init() so
// the registration site stays close to the handler — the dispatcher
// in tools.go only needs the single name → handler entry.
//
// Config files live on the tmux server (not on a session), so this
// tool deliberately is not session-scoped: there is no `session`
// field in the schema and SessionPrefix does not apply.
var sourceFileToolDefs = []map[string]any{
	{
		"name": "source_file",
		"description": "Re-source a tmux config file via `tmux source-file PATH`. Useful for hot-reloading " +
			"tweaks (status bar, key bindings, options) without restarting the tmux server. " +
			"`path` must be an absolute filesystem path; the boundary rejects relative paths, " +
			"control characters, and `..` traversal segments before tmux is consulted. Set " +
			"`quiet=true` to map to `-q`, which tells tmux to suppress non-fatal errors so a " +
			"partially-incompatible config still reloads as far as it can. The response is a " +
			"small JSON ack `{\"sourced\": true}` on success; the actual reload effects are " +
			"observable via `show_options`.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute filesystem path to the tmux.conf file. Max 4096 bytes; rejects control characters and `..` segments.",
				},
				"quiet": map[string]any{
					"type":        "boolean",
					"description": "When true, pass `-q` so tmux suppresses non-fatal errors (unknown options, missing file). Defaults to false.",
					"default":     false,
				},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register source_file onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, sourceFileToolDefs...)
}

// validateSourceFilePath enforces the conservative path policy for
// source_file's required `path` argument. Empty is rejected (tmux
// would otherwise treat "" as a positional and emit a confusing
// error); a non-empty value must:
//
//   - fit inside the 4096-byte ceiling so a hostile caller can't pin
//     argv copying multi-MB strings;
//   - contain no NUL byte (a NUL would silently truncate the argv
//     entry tmux receives — security-relevant);
//   - contain no other control characters (`unicode.IsControl`),
//     including stray newlines/tabs that could confuse tmux's own
//     error formatting;
//   - be absolute, so the behaviour does not depend on whatever
//     working directory the tmux-mcp binary was launched from
//     (systemd / container deployments routinely chdir to /);
//   - not contain a `..` traversal segment, so a hostile caller
//     can't combine an absolute prefix with parent traversal to
//     reach a sibling tree (`/etc/tmux-mcp/../passwd`).
//
// The check returns CodeInvalidParams (-32602) so the JSON-RPC layer
// emits a clean diagnostic before any tmux command runs.
func validateSourceFilePath(path string) *rpcError {
	if path == "" {
		return invalidParams("path required")
	}
	if len(path) > maxSourceFilePathLen {
		return invalidParams("path length %d out of range [1..%d]", len(path), maxSourceFilePathLen)
	}
	for i, r := range path {
		if r == 0 {
			return invalidParams("path contains NUL byte at offset %d", i)
		}
		if unicode.IsControl(r) {
			return invalidParams("path contains control character %U at offset %d", r, i)
		}
	}
	if !filepath.IsAbs(path) {
		return invalidParams("path %q must be absolute (e.g. /etc/tmux-mcp/tmux.conf)", path)
	}
	// Walk the path's components ourselves (rather than relying on
	// filepath.Clean) because Clean would silently collapse `..` for
	// us — we want the rejection to fire so a caller composing
	// `/etc/tmux-mcp/../etc/passwd` sees the error rather than a
	// silently-resolved path. SplitList is for ":" separators (PATH);
	// the right call for traversal segments is to split on the path
	// separator and inspect each part.
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == ".." {
			return invalidParams("path %q must not contain `..` traversal segment", path)
		}
	}
	return nil
}

// sourceFile drives tmuxctl.Controller.SourceFile. The handler
// validates the required `path` argument up front so a malformed
// value (relative path, control char, traversal) sees
// CodeInvalidParams (-32602) before any tmux command runs. The
// response is a small JSON ack `{"sourced": true}`; the boundary
// deliberately does not echo the loaded line count because tmux
// source-file reports nothing of the sort and a follow-up
// show_options is one call away if the agent wants to confirm the
// reload took effect.
func (t *Tools) sourceFile(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Path  string `json:"path"`
		Quiet bool   `json:"quiet"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("source_file: %v", err)
	}
	if rerr := validateSourceFilePath(args.Path); rerr != nil {
		return nil, invalidParams("source_file: %s", rerr.Message)
	}
	if err := t.Ctl.SourceFile(ctx, args.Path, args.Quiet); err != nil {
		return nil, internalError(fmt.Errorf("source_file: %w", err))
	}
	return jsonBlock(map[string]any{"sourced": true})
}

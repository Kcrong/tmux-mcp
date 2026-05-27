package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// maxSuspendClientTargetLen caps the optional `target_client` argument
// so a hostile or buggy caller cannot blow up tmux's argv with a
// megabyte payload before the boundary even runs. Real tmux client
// targets are short (a TTY path like "/dev/pts/3", a session-relative
// reference like "demo:0", or an internal client id like "%client-1");
// 256 bytes is generous while staying well under any reasonable shell-
// arg cap.
const maxSuspendClientTargetLen = 256

// suspendClientTargetRE is the conservative allowlist for `-t` values.
// tmux accepts a wide range of client-target shapes — TTY paths,
// session-qualified names, internal "%foo" handles — and the regex
// covers the realistic union: alphanumerics, slash (TTY paths), dash
// and underscore (allowed in session names), colon (session:window
// targets), period (pane indices), and percent (internal handles).
// Anything outside the set is almost certainly a typo or an attempt to
// smuggle a shell metachar into argv; refuse it at the boundary.
var suspendClientTargetRE = regexp.MustCompile(`^[A-Za-z0-9_./:%-]+$`)

// suspendClientToolDefs holds the JSON Schema for the suspend_client
// tool. It is appended onto the main toolDefs slice from this file's
// init() so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
//
// `target_client` is schema-optional because tmux accepts a bare
// `suspend-client` call (which it resolves to the "current" client).
// On the headless servers tmux-mcp typically owns that lookup falls
// back to a clean no-op (see SuspendClient), so the tool's empty-args
// shape is also a meaningful "suspend whoever is watching, if anyone"
// fire-and-forget primitive.
//
// Locking additionalProperties keeps the schema strict so an agent
// that misnames the field (e.g. "client" instead of "target_client")
// gets a fast schema-shaped rejection rather than a silent no-op.
var suspendClientToolDefs = []map[string]any{
	{
		"name": "suspend_client",
		"description": "Suspend a tmux client (send SIGTSTP) via `tmux suspend-client [-t target-client]`. " +
			"Strictly less destructive sibling of `detach_client`: the client process is paused " +
			"and can be resumed with `fg`, while the session itself stays intact and unattached " +
			"clients are unaffected. Pass `target_client` to suspend a specific client (TTY path " +
			"like `/dev/pts/3`, a session reference like `demo:0`, or an internal `%client-1` " +
			"handle); omit it to let tmux pick the \"current\" client. On the headless tmux " +
			"servers tmux-mcp typically owns there is no current client, so an empty-args call " +
			"is a clean no-op (rather than an error). A `target_client` that does not match any " +
			"attached client surfaces as `-32000` (errs.ErrSessionNotFound), mirroring the " +
			"contract list_clients / detach_client uphold.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target_client": map[string]any{
					"type":        "string",
					"maxLength":   maxSuspendClientTargetLen,
					"description": "Optional client target (TTY path, session-qualified name, or internal `%client-id`); maps to `-t TARGET-CLIENT`. Omit to suspend tmux's \"current\" client (a clean no-op when nobody is attached).",
				},
			},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register suspend_client onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, suspendClientToolDefs...)
}

// validateSuspendClientTarget enforces the regex/length policy on the
// optional `target_client` argument. Empty is allowed (the "no -t
// flag" shape that tmux resolves to the current client); a non-empty
// value must satisfy suspendClientTargetRE so a stray quote, control
// byte, or path-injection attempt cannot reach tmux's argv.
func validateSuspendClientTarget(target string) *rpcError {
	if target == "" {
		return nil
	}
	if len(target) > maxSuspendClientTargetLen {
		return invalidParams(
			"suspend_client: target_client length %d out of range [1..%d]",
			len(target), maxSuspendClientTargetLen,
		)
	}
	if !suspendClientTargetRE.MatchString(target) {
		return invalidParams(
			"suspend_client: target_client %q must match %s",
			target, suspendClientTargetRE.String(),
		)
	}
	return nil
}

// suspendClient drives tmuxctl.Controller.SuspendClient. The handler is
// deliberately small — the boundary's only job is to validate the
// optional `target_client` argument and forward the call. The
// underlying controller method handles the headless-no-op and named-
// missing-target shapes uniformly so this layer never has to substring-
// match tmux stderr.
//
// Response is a small JSON ack (`{"suspended": true}`) so callers that
// chain suspend_client with another mutation can branch on a stable
// shape rather than parse a free-form status string. The headless no-op
// case (no clients attached server-wide, empty target_client) returns
// the same ack — tmux itself does not distinguish that case from a
// successful suspend, and surfacing the difference would push every
// caller to write a "did anything actually pause?" branch they do not
// need.
func (t *Tools) suspendClient(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		TargetClient string `json:"target_client"`
	}
	// Allow an absent / empty arguments object — the empty-args shape
	// is meaningful here ("suspend whoever is current, if anyone").
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("suspend_client: %v", err)
		}
	}
	if rerr := validateSuspendClientTarget(args.TargetClient); rerr != nil {
		return nil, rerr
	}
	opts := tmuxctl.SuspendClientOpts{TargetClient: args.TargetClient}
	if err := t.Ctl.SuspendClient(ctx, opts); err != nil {
		return nil, internalError(fmt.Errorf("suspend_client: %w", err))
	}
	return jsonBlock(map[string]any{"suspended": true})
}

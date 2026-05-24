package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// respawnPaneToolDefs holds the JSON Schema for the respawn_pane tool.
// It is appended onto the main toolDefs slice from this file's init()
// so the registration site stays close to the handler — the dispatcher
// in tools.go only needs the single name → handler entry.
var respawnPaneToolDefs = []map[string]any{
	{
		"name": "respawn_pane",
		"description": "Restart the command in a tmux pane via `tmux respawn-pane [-k] -t " +
			"<session>:<window>.<pane> [command]`. Useful when the foreground command in a pane " +
			"has exited (e.g. a build watcher crashed) and the agent wants to bring it back " +
			"without recreating the pane — the pane id, layout slot, and surrounding window " +
			"remain unchanged. When `command` is omitted tmux re-runs whatever was used to start " +
			"the pane originally (typically the user's default shell). When `command` is set it " +
			"is forwarded as the trailing argv to `tmux respawn-pane` and run via `/bin/sh -c`. " +
			"`kill` (default false) maps to tmux's `-k` flag: when true tmux SIGKILLs the " +
			"existing process before starting the new one; when false tmux refuses the call if " +
			"the pane is still active and the boundary returns CodePaneActive (-32005) so the " +
			"client can retry with kill=true. Returns `{\"respawned\": true}` on success.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "Session id; len 1-64, [A-Za-z0-9_-].",
				},
				"window": map[string]any{
					"type":        "string",
					"description": "Window name (1-64, [A-Za-z0-9_-]) or numeric index (\\d+).",
				},
				"pane": map[string]any{
					"type":        "string",
					"description": "Pane index (\\d+) or tmux `%N` pane id.",
				},
				"command": map[string]any{
					"type":        "string",
					"description": "Optional command to run in the respawned pane; defaults to the original.",
				},
				"kill": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, kill the running process before respawning (tmux -k).",
				},
			},
			"required":             []string{"session", "window", "pane"},
			"additionalProperties": false,
		},
	},
}

// paneIndexRE accepts either a numeric pane index (`0`, `12`) or a
// tmux pane id of the form `%N`. Unlike the broader paneTargetRE used
// for `target_pane` arguments this one rejects embedded session/window
// segments — respawn_pane keeps session and window in their own fields
// so the controller assembles the full `<session>:<window>.<pane>`
// target from validated parts and a stray colon in the `pane` field
// would either round-trip into the target string verbatim or duplicate
// the session/window addressing already supplied. Mirrors the schema
// description exactly so tooling (json-schema → docs) and the runtime
// guard cannot drift.
var paneIndexRE = regexp.MustCompile(`^([0-9]+|%[0-9]+)$`)

// maxPaneIndexLen pins the upper length bound on respawn_pane's
// `pane` argument. tmux numeric pane indices are tiny in practice
// (single or double digits) and `%N` ids stay short for the lifetime
// of the server, so 32 bytes is well above any legitimate value while
// still bounding the pre-tmux input.
const maxPaneIndexLen = 32

// validatePaneIndex enforces the respawn_pane `pane` policy: a
// non-empty value matching paneIndexRE within the length cap. Mirrors
// validateWindowTarget's "required, regex, length" shape so callers
// see a uniform error envelope across the pane tool surface.
func validatePaneIndex(pane string) *rpcError {
	if pane == "" {
		return invalidParams("pane required")
	}
	if len(pane) > maxPaneIndexLen {
		return invalidParams("pane length %d out of range [1..%d]", len(pane), maxPaneIndexLen)
	}
	if !paneIndexRE.MatchString(pane) {
		return invalidParams("pane %q must match %s", pane, paneIndexRE.String())
	}
	return nil
}

// maxRespawnCommandLen caps the optional `command` argument passed to
// respawn_pane. tmux happily forwards arbitrary strings to /bin/sh -c,
// but a realistic respawn rarely exceeds a few hundred bytes — anything
// past 4 KiB is almost certainly a buggy or hostile caller, and bounding
// it here keeps the JSON-RPC frame size predictable. Mirrors the
// pane_split cap so the two pane-creating tools share the same policy.
const maxRespawnCommandLen = 4096

func init() {
	// Register respawn_pane onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, respawnPaneToolDefs...)
}

// respawnPane drives tmuxctl.Controller.RespawnPane. The handler does
// the same up-front validation as the rest of the pane tool surface so
// callers get a CodeInvalidParams response before any tmux command runs.
//
// Validation:
//   - session: required, regex/length policy via validateSessionRef.
//   - window:  required, name (1-64, [A-Za-z0-9_-]) or numeric index.
//   - pane:    required, numeric index or tmux `%N` id.
//   - command: optional; bounded length, no newlines (tmux respawn-pane
//     would treat them as argv separators on /bin/sh -c, breaking the
//     "single command" contract).
//   - kill:    optional, defaults to false.
//
// A pane that is still running its original command (when kill=false)
// surfaces as errs.ErrPaneActive → CodePaneActive (-32005) so the client
// can branch on the code and retry with kill=true.
func (t *Tools) respawnPane(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
		Window  string `json:"window"`
		Pane    string `json:"pane"`
		Command string `json:"command"`
		Kill    bool   `json:"kill"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("respawn_pane: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if rerr := validateWindowTarget(args.Window); rerr != nil {
		return nil, invalidParams("respawn_pane: %s", rerr.Message)
	}
	if rerr := validatePaneIndex(args.Pane); rerr != nil {
		return nil, invalidParams("respawn_pane: %s", rerr.Message)
	}
	if args.Command != "" {
		if strings.ContainsAny(args.Command, "\n\r") {
			return nil, invalidParams("command: must not contain newlines")
		}
		if len(args.Command) > maxRespawnCommandLen {
			return nil, invalidParams(
				"respawn_pane: command length %d exceeds %d",
				len(args.Command), maxRespawnCommandLen,
			)
		}
	}
	if err := t.Ctl.RespawnPane(
		ctx,
		args.Session,
		args.Window,
		args.Pane,
		args.Command,
		args.Kill,
	); err != nil {
		return nil, internalError(fmt.Errorf("respawn_pane: %w", err))
	}
	return jsonBlock(map[string]any{"respawned": true})
}

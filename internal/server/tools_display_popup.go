package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// maxDisplayPopupFreeFormLen caps every free-form string argument the
// display_popup tool forwards into tmux: title, border_style,
// shell_command, and the env keys/values. tmux happily parses long
// format/style strings (the DSL is recursive), but a realistic popup
// configuration never exceeds a few hundred bytes — anything past
// 4 KiB is almost certainly a typo or hostile caller, and bounding
// here keeps the JSON-RPC frame size predictable.
const maxDisplayPopupFreeFormLen = 4096

// maxDisplayPopupSizeLen caps the optional width/height/x/y string
// arguments. tmux accepts either a percentage ("80%") or a number of
// cells; both are short. 32 bytes is generous while staying well under
// any plausible legitimate value.
const maxDisplayPopupSizeLen = 32

// maxDisplayPopupEnvEntries caps the number of environment-variable
// entries a single display_popup call may pass. tmux's command-line
// length is bounded but generous; capping here keeps a buggy or
// hostile caller from inflating argv with thousands of -e flags.
const maxDisplayPopupEnvEntries = 64

// borderLinesRE matches the documented vocabulary tmux's
// `popup-border-lines` option accepts (single, double, heavy, simple,
// rounded, padded, none). We restrict to alnum/dash so a malformed
// value gets rejected up front instead of reaching tmux's argv where
// the diagnostic is version-dependent.
var borderLinesRE = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// popupSizeRE accepts the two shapes tmux's `-h`/`-w`/`-x`/`-y`
// arguments take: a non-negative integer (number of cells / row or
// column index) or the same followed by a percent sign. Anything
// else is refused before the call reaches tmux so a stray glob /
// metachar cannot slip through.
var popupSizeRE = regexp.MustCompile(`^[0-9]+%?$`)

// envNameRE matches the environment-variable names display_popup will
// forward via `-e KEY=VALUE`. POSIX restricts env names to
// alphanumerics + underscore with a non-digit first character; the
// regex below is the standard transcription. tmux itself does not
// validate the name shape — the boundary does so a caller cannot
// inject a stray `=` or whitespace into argv.
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// displayPopupToolDefs holds the JSON Schema for the display_popup
// tool. It is appended onto the main toolDefs slice from this file's
// init() so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
var displayPopupToolDefs = []map[string]any{
	{
		"name": "display_popup",
		"description": "Render an overlay popup on the attached tmux client via " +
			"`tmux display-popup [-BCE] [-b border-lines] [-d start-directory] " +
			"[-e VAR=value] [-h height] [-w width] [-x position] [-y position] " +
			"[-r] [-T title] [-S border-style] [-t target-pane] [shell-command]`. " +
			"The popup is a rectangular overlay drawn on top of any panes; pane " +
			"contents are not refreshed while the popup is visible. Sizing knobs " +
			"(`width` / `height`) accept either a percentage (\"80%\") or a number " +
			"of cells; omit them to let tmux centre a half-the-terminal popup. " +
			"`shell_command` runs inside the popup; omit it to launch the user's " +
			"shell. Pair with `close_on_exit` (`-C`) or `close_on_zero_exit` " +
			"(`-E`) to make the popup self-dismiss on exit. The tool refuses to " +
			"run when the named target does not exist (`-32000`) and propagates " +
			"any other tmux error verbatim under `-32603` — including the " +
			"\"no current client\" surface that fires on a headless server, " +
			"which the boundary deliberately surfaces so an operator notices " +
			"the daemon has nothing to draw on.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Optional pane target (\"session\", \"session:window\", \"session:window.pane\", or `%N`).",
				},
				"title": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayPopupFreeFormLen,
					"description": "tmux format string used for the popup title (-T).",
				},
				"border_style": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayPopupFreeFormLen,
					"description": "tmux style spec applied to the popup border (-S), e.g. \"fg=red\".",
				},
				"border_lines": map[string]any{
					"type":        "string",
					"maxLength":   32,
					"description": "Glyph set tmux uses to draw the popup border (-b); see popup-border-lines.",
				},
				"start_directory": map[string]any{
					"type":        "string",
					"description": "Absolute path tmux uses as the popup shell-command's cwd (-d).",
				},
				"env": map[string]any{
					"type": "object",
					"additionalProperties": map[string]any{
						"type":      "string",
						"maxLength": maxDisplayPopupFreeFormLen,
					},
					"description": "Environment overrides forwarded as repeated -e KEY=VALUE pairs.",
				},
				"width": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayPopupSizeLen,
					"description": "Popup width as cells (\"60\") or percentage (\"60%\"); maps to -w.",
				},
				"height": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayPopupSizeLen,
					"description": "Popup height as cells (\"20\") or percentage (\"50%\"); maps to -h.",
				},
				"x": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayPopupSizeLen,
					"description": "Popup x-position (-x); same shape as width.",
				},
				"y": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayPopupSizeLen,
					"description": "Popup y-position (-y); same shape as height.",
				},
				"shell_command": map[string]any{
					"type":        "string",
					"maxLength":   maxDisplayPopupFreeFormLen,
					"description": "Optional shell-command tmux runs inside the popup; defaults to the user's shell.",
				},
				"no_border": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, suppress the popup border (-B). Overrides border_lines / border_style on tmux.",
				},
				"close_on_exit": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, close the popup when shell_command exits (-C).",
				},
				"close_on_zero_exit": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, close the popup only when shell_command exits 0 (-E).",
				},
				"centered": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, request tmux to centre the popup (-r). Requires tmux >= 3.5.",
				},
			},
			// display_popup's surface is locked to the documented set;
			// an unknown field is far more likely a typo (e.g.
			// "border-lines" with a dash) than a future capability we
			// forgot to advertise, so reject it up front rather than
			// silently ignore it.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register display_popup onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from the single dispatcher case in tools.go)
	// and avoids touching the shared toolDefs literal that other PRs
	// are editing. The tool mutates client UI state, so it is
	// deliberately not added to readonly.go's allowlist — read-only
	// deployments will see a -32011 rejection from the dispatcher.
	toolDefs = append(toolDefs, displayPopupToolDefs...)
}

// validateDisplayPopupFreeForm enforces the conservative shape on the
// free-form string arguments (title, border_style, shell_command).
// Newlines are rejected because they would split tmux's title bar
// across multiple rows — and, for shell_command, would dump the
// trailing portion as a separate command tmux happily executes. The
// length cap is the standard 4 KiB JSON-RPC frame guard.
func validateDisplayPopupFreeForm(field, value string) *rpcError {
	if value == "" {
		return nil
	}
	if len(value) > maxDisplayPopupFreeFormLen {
		return invalidParams("%s length %d exceeds %d", field, len(value), maxDisplayPopupFreeFormLen)
	}
	if strings.ContainsAny(value, "\n\r") {
		return invalidParams("%s: must not contain newlines", field)
	}
	return nil
}

// validateDisplayPopupSize enforces the popupSizeRE shape on the
// width / height / x / y arguments. tmux accepts either a number of
// cells or a percentage; refusing anything else here keeps a stray
// "auto" / "%50" from reaching tmux where the diagnostic is
// version-dependent.
func validateDisplayPopupSize(field, value string) *rpcError {
	if value == "" {
		return nil
	}
	if len(value) > maxDisplayPopupSizeLen {
		return invalidParams("%s length %d exceeds %d", field, len(value), maxDisplayPopupSizeLen)
	}
	if !popupSizeRE.MatchString(value) {
		return invalidParams("%s %q must match %s", field, value, popupSizeRE.String())
	}
	return nil
}

// validateDisplayPopupBorderLines bounds the border_lines argument to
// the conservative regex above. We do not enforce the full tmux
// vocabulary because future tmux versions may grow the option set;
// the regex is loose enough to admit any plausible new value while
// still refusing shell metachars.
func validateDisplayPopupBorderLines(value string) *rpcError {
	if value == "" {
		return nil
	}
	if len(value) > 32 {
		return invalidParams("border_lines length %d exceeds 32", len(value))
	}
	if !borderLinesRE.MatchString(value) {
		return invalidParams("border_lines %q must match %s", value, borderLinesRE.String())
	}
	return nil
}

// validateDisplayPopupEnv enforces the env map shape. POSIX names
// (alnum + underscore, non-digit first character) keep a stray `=`
// from breaking the `KEY=VALUE` pairing tmux expects. Values bound
// by the same 4 KiB cap as title / shell_command, and the entry count
// is capped overall so a hostile caller cannot inflate argv with
// thousands of -e flags.
func validateDisplayPopupEnv(env map[string]string) *rpcError {
	if len(env) == 0 {
		return nil
	}
	if len(env) > maxDisplayPopupEnvEntries {
		return invalidParams("env entries %d exceeds %d", len(env), maxDisplayPopupEnvEntries)
	}
	for k, v := range env {
		if k == "" {
			return invalidParams("env: empty key")
		}
		if !envNameRE.MatchString(k) {
			return invalidParams("env key %q must match %s", k, envNameRE.String())
		}
		if len(v) > maxDisplayPopupFreeFormLen {
			return invalidParams("env[%q] length %d exceeds %d", k, len(v), maxDisplayPopupFreeFormLen)
		}
		if strings.ContainsAny(v, "\n\r") {
			return invalidParams("env[%q]: must not contain newlines", k)
		}
	}
	return nil
}

// displayPopup drives [tmuxctl.Controller.DisplayPopup] and serialises
// the result to the standard `{"content":[{"type":"text","text":"<json>"}]}`
// envelope MCP expects from a tools/call. The response shape is a flat
// `{"opened": true}` ack; the boundary deliberately does not echo the
// flag values back because display-popup is a fire-and-forget UX
// trigger — a follow-up call is one tool away if the agent wants
// confirmation.
//
// Up-front validation:
//   - `target` (when set) must satisfy paneTargetRE so a stray quote
//     or shell metachar cannot reach tmux;
//   - `start_directory` must be absolute (matches the
//     session_create / window_create policy);
//   - free-form strings (title, border_style, shell_command) refuse
//     newlines and bound to 4 KiB;
//   - sizing strings (width, height, x, y) match the popupSizeRE
//     vocabulary;
//   - env entries are POSIX-shaped and capped at 64 pairs.
//
// Unknown targets surface via the wrapped errs.ErrSessionNotFound the
// controller emits, which the JSON-RPC layer maps to
// CodeSessionNotFound (-32000). Other tmux failures (no current
// client, unknown flag on older tmux) propagate via CodeInternal so
// operators notice the underlying issue.
func (t *Tools) displayPopup(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target          string            `json:"target"`
		Title           string            `json:"title"`
		BorderStyle     string            `json:"border_style"`
		BorderLines     string            `json:"border_lines"`
		StartDirectory  string            `json:"start_directory"`
		Env             map[string]string `json:"env"`
		Width           string            `json:"width"`
		Height          string            `json:"height"`
		X               string            `json:"x"`
		Y               string            `json:"y"`
		ShellCommand    string            `json:"shell_command"`
		NoBorder        bool              `json:"no_border"`
		CloseOnExit     bool              `json:"close_on_exit"`
		CloseOnZeroExit bool              `json:"close_on_zero_exit"`
		Centered        bool              `json:"centered"`
	}
	// json.Unmarshal on an empty payload is fine — every
	// display_popup argument is optional, so `arguments: {}` (or
	// missing arguments entirely) is a valid call that maps onto the
	// bare `tmux display-popup` command. Mirrors choose_tree /
	// list_clients here.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("display_popup: %v", err)
		}
	}
	if args.Target != "" {
		if rerr := validatePaneTarget(args.Target); rerr != nil {
			return nil, invalidParams("display_popup: %s", rerr.Message)
		}
	}
	if args.StartDirectory != "" && !filepath.IsAbs(args.StartDirectory) {
		return nil, invalidParams("start_directory %q must be an absolute path", args.StartDirectory)
	}
	for _, f := range []struct {
		name, value string
	}{
		{"title", args.Title},
		{"border_style", args.BorderStyle},
		{"shell_command", args.ShellCommand},
	} {
		if rerr := validateDisplayPopupFreeForm(f.name, f.value); rerr != nil {
			return nil, rerr
		}
	}
	if rerr := validateDisplayPopupBorderLines(args.BorderLines); rerr != nil {
		return nil, rerr
	}
	for _, f := range []struct {
		name, value string
	}{
		{"width", args.Width},
		{"height", args.Height},
		{"x", args.X},
		{"y", args.Y},
	} {
		if rerr := validateDisplayPopupSize(f.name, f.value); rerr != nil {
			return nil, rerr
		}
	}
	if rerr := validateDisplayPopupEnv(args.Env); rerr != nil {
		return nil, rerr
	}
	opts := tmuxctl.DisplayPopupOptions{
		Target:          t.resolvePaneTarget(args.Target),
		Title:           args.Title,
		BorderStyle:     args.BorderStyle,
		BorderLines:     args.BorderLines,
		StartDirectory:  args.StartDirectory,
		Env:             args.Env,
		Width:           args.Width,
		Height:          args.Height,
		X:               args.X,
		Y:               args.Y,
		ShellCommand:    args.ShellCommand,
		NoBorder:        args.NoBorder,
		CloseOnExit:     args.CloseOnExit,
		CloseOnZeroExit: args.CloseOnZeroExit,
		Centered:        args.Centered,
	}
	if err := t.Ctl.DisplayPopup(ctx, opts); err != nil {
		return nil, internalError(fmt.Errorf("display_popup: %w", err))
	}
	return jsonBlock(map[string]any{"opened": true})
}

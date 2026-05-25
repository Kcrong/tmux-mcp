package server

// readOnlyTools is the set of tool names the dispatcher will accept when
// the operator has armed -read-only. Membership is the policy: anything
// in this set is treated as a pure inspection of tmux state and may run;
// anything else is rejected with [errs.CodeReadOnly] before any handler
// touches the controller.
//
// The list intentionally covers both the names this server actually
// registers today (e.g. "capture", "session_list", "display_message")
// and the alternate names the read-only feature spec calls out
// ("capture_pane", "list_sessions", "show_message", plus the
// list_buffers / show_buffer / show_options surfaces a future
// contributor might add). Keeping the spec-named entries in the same
// allowlist means a future tool registered under one of those names is
// automatically inspection-allowed without a second policy edit; if it
// turns out to mutate state, removing the entry is a localised change
// no caller of [IsReadOnlyTool] needs to know about.
//
// The map is package-private; the dispatcher consults it through
// [IsReadOnlyTool] so the allowlist lives in exactly one file
// (readonly.go) and tests / future code can't drift from the policy by
// re-defining the set.
var readOnlyTools = map[string]struct{}{
	// Pane / session capture — read-only, returns whatever tmux
	// currently shows. "capture" is the registered name; "capture_pane"
	// is the conventional MCP name and the one the read-only spec
	// reserves for future renames or aliases.
	"capture":      {},
	"capture_pane": {},
	// Wait-for-text polls the visible pane until a regex matches, but
	// never types into the session. wait_for_stable is deliberately
	// excluded — the spec lists wait_for_text only — so a read-only
	// agent that wants a "settle" primitive must request the more
	// targeted text match instead of an unbounded "wait for nothing
	// to change" wait.
	"wait_for_text": {},
	// Listings: every list_* / *_list tool returns metadata about the
	// existing tmux state without modifying it.
	"session_list":  {},
	"list_sessions": {},
	"list_panes":    {},
	"list_windows":  {},
	"list_clients":  {},
	"list_buffers":  {},
	"list_keys":     {},
	// choose_tree is the snapshot form of `tmux choose-tree` — it
	// only ever runs `tmux list-windows -F ...` under the hood and
	// never mutates server state.
	"choose_tree": {},
	// find_window is a pure search over already-existing windows: it
	// runs `tmux list-windows -F … -f <filter>` and returns the matching
	// rows, never mutating tmux state. Belongs in the read-only
	// allowlist alongside list_windows so an inspection-only agent can
	// still locate a target by name / title / content.
	"find_window": {},
	// Buffer / option / message inspectors. show_buffer and
	// show_options are spec-named for forward compatibility — neither
	// is registered today, but adding them here means the policy is
	// already correct on the day they land. save_buffer is the
	// canonical "I want the whole buffer payload" read path; it is
	// inspection-only by construction (it never mutates tmux state)
	// so it lives next to show_buffer here.
	"show_buffer":  {},
	"save_buffer":  {},
	"show_options": {},
	// show_window_options is the read-side sibling of set_window_option:
	// it wraps `tmux show-window-options [-g] [-t TARGET] [OPTION]` and
	// only ever inspects the resolved window-options table. No write
	// path runs in this handler, so it belongs on the inspection
	// allowlist alongside show_options.
	"show_window_options": {},
	// "display_message" is the registered tool name; "show_message" is
	// the spec-named alias the read-only feature reserves so callers
	// targeting either name see the same policy.
	"display_message": {},
	"show_message":    {},
	// show_messages reads tmux's per-client message log
	// (`tmux show-messages [-JT]`) — a pure inspection of the buffer
	// tmux maintains for the bottom status bar. Note the trailing 's':
	// distinct from the "show_message" alias above (which is the
	// inspection-only spelling of display_message).
	"show_messages": {},
	// Per-session metadata views — describe / inspect both run a
	// `display-message`-style read against tmux and never mutate.
	"session_describe": {},
	"session_inspect":  {},
	// has_session is a one-bit existence probe (`tmux has-session -t
	// NAME`); strictly cheaper than session_list and inspection-only
	// by construction.
	"has_session": {},
}

// IsReadOnlyTool reports whether name is allowed when the server is
// running with -read-only. The policy lives in [readOnlyTools]; this
// helper is the only export so callers (the dispatcher today, future
// telemetry / introspection code tomorrow) cannot bypass the table by
// hard-coding their own list. An empty name returns false so a
// malformed tools/call (no name field) cannot sneak past the gate —
// the dispatcher already rejects empty names through the static
// switch's fallthrough, but the centralised check here keeps the
// contract uniform for every call site.
func IsReadOnlyTool(name string) bool {
	if name == "" {
		return false
	}
	_, ok := readOnlyTools[name]
	return ok
}

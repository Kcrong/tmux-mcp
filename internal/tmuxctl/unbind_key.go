package tmuxctl

import (
	"context"
	"errors"
	"strings"
)

// UnbindKey wraps `tmux unbind-key [-a] [-T TABLE] [KEY]`. It removes a
// single key binding when key is set, or every binding in a key table
// when all is true.
//
// Argument shape:
//   - all=true  → `-a`. Asks tmux to remove every binding in the named
//     table (or the default key table when table is empty). MUST NOT be
//     combined with a non-empty key — tmux's `-a` and a positional KEY
//     argument contradict each other; the boundary refuses the call up
//     front so callers see a clean argument-shape error rather than
//     tmux's version-dependent stderr.
//   - table != "" → `-T TABLE`. Scopes the removal to a single keymap
//     (e.g. "prefix", "root", "copy-mode", "copy-mode-vi"). Empty means
//     "no `-T` flag" so tmux uses its default table for the operation.
//   - key  != ""  → positional `KEY` argument. The chord to remove
//     (e.g. "C-a", "Up", "M-{").
//
// Validation. Exactly one of {key set, all=true} is required. Both
// empty would silently no-op on tmux and is treated here as a programmer
// error rather than degrading to a meaningless call. Both set is
// refused for the same reason — a future tmux that started honouring
// `-a KEY` would do something surprising and silent, and callers should
// pick one shape explicitly.
//
// Idempotent semantics. The goal state is "this binding is not
// present"; if it was never bound (or a prior call already removed it)
// the call is a no-op, not an error. tmux's `unbind-key` itself is
// fussier than this contract suggests — across the supported version
// range it emits non-zero exits with a stderr diagnostic for two
// "already-gone" shapes:
//
//   - "table TABLE doesn't exist" — when the targeted key table has
//     no live bindings (e.g. a custom user table whose last binding
//     was just removed; tmux deletes the empty table from its lookup
//     map and a subsequent `-T TABLE` invocation finds nothing).
//   - "unknown key: KEY" — when the named key chord is not currently
//     bound in the targeted (existing) table. tmux only knows whether
//     a chord *is* bound, not whether it ever was.
//
// Both phrases are recognised here so a caller's recovery loop that
// re-issues the same setup/teardown frames does not see a spurious
// failure on the second iteration. Any other failure (tmux not on
// PATH, an IO error, the daemon dying mid-call) propagates through
// unchanged so the JSON-RPC layer can map it to CodeInternal.
func (c *Controller) UnbindKey(ctx context.Context, key, table string, all bool) error {
	switch {
	case all && key != "":
		return errors.New("unbind_key: -a (all) cannot be combined with a non-empty key")
	case !all && key == "":
		return errors.New("unbind_key: must set either key or all=true")
	}
	args := []string{"unbind-key"}
	if all {
		// `-a` removes every binding in the targeted table. tmux still
		// honours `-T TABLE` alongside `-a` to scope the wipe; without
		// `-T`, the wipe targets tmux's default table for the operation.
		args = append(args, "-a")
	}
	if table != "" {
		// `-T TABLE` scopes both the single-key removal (when key is
		// set) and the all-keys wipe (when all is true). Forwarding it
		// uniformly across both shapes keeps the boundary additive.
		args = append(args, "-T", table)
	}
	if key != "" {
		// The positional KEY argument follows tmux's option flags. We
		// pass it through verbatim — tmux is the source of truth on
		// what counts as a valid keysym, and the boundary's job is
		// just to keep it bounded (length / control bytes) before it
		// reaches argv.
		args = append(args, key)
	}
	if _, err := c.run(ctx, args...); err != nil {
		// Match the two "already-gone" stderr phrases case-
		// insensitively so a future tmux that capitalised the
		// diagnostic does not regress the idempotent contract. Any
		// other failure shape propagates verbatim.
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "table ") && strings.Contains(lower, "doesn't exist") {
			return nil
		}
		if strings.Contains(lower, "unknown key") {
			return nil
		}
		return err
	}
	return nil
}

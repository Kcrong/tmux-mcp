#!/usr/bin/env bash
# Apply the canonical branch ruleset for Kcrong/tmux-mcp.
#
# Why this script exists:
#   - The repo's main-branch protection lives as a GitHub Ruleset (id 16797918,
#     name "default"), not the legacy branch-protection API.
#   - Updating it from CI would need a PAT with admin:repo scope, which we
#     deliberately do NOT bundle. Any maintainer can re-run this from a
#     local checkout instead.
#
# Requirements:
#   - gh CLI authenticated as a repo admin (`gh auth status` shows the
#     "admin:repo_hook"/"repo" scopes), OR
#   - a PAT with admin:repo scope exported as GH_TOKEN.
#
# Usage:
#   ./scripts/apply-ruleset.sh                       # uses the default ruleset id
#   RULESET_ID=12345 ./scripts/apply-ruleset.sh      # override for forks
#   PAYLOAD=other.json ./scripts/apply-ruleset.sh    # use a different JSON file
#
# Idempotency: a successful PUT replaces the ruleset's name/target/enforcement/
# conditions/bypass_actors/rules with the contents of the JSON payload. Running
# this script with the same payload twice is a no-op modulo the `updated_at`
# timestamp on the server side.
set -euo pipefail

REPO="${REPO:-Kcrong/tmux-mcp}"
RULESET_ID="${RULESET_ID:-16797918}"
PAYLOAD="${PAYLOAD:-scripts/ruleset-default.json}"

if [ ! -f "$PAYLOAD" ]; then
  echo "error: payload not found: $PAYLOAD" >&2
  exit 1
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "error: gh CLI is required (https://cli.github.com/)" >&2
  exit 1
fi

# Validate JSON locally before we make a network call. `jq -e` returns
# non-zero on invalid JSON or empty payload, which short-circuits the PUT.
if command -v jq >/dev/null 2>&1; then
  jq -e . "$PAYLOAD" >/dev/null
else
  echo "warn: jq not installed; skipping local JSON validation" >&2
fi

echo "Applying ruleset $RULESET_ID on $REPO from $PAYLOAD ..."
gh api \
  --method PUT \
  -H "Accept: application/vnd.github+json" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  "repos/$REPO/rulesets/$RULESET_ID" \
  --input "$PAYLOAD"

echo "Done. Verify in the UI: https://github.com/$REPO/rules/$RULESET_ID"

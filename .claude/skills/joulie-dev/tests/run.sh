#!/usr/bin/env bash
# Behavioural tests for the joulie-dev skill. See README.md next to this file.
#
#   run.sh [--baseline] [--model MODEL] [scenario-name ...]
#
# Exit status: number of failed scenarios.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../../.." && pwd)"
SCENARIOS="$HERE/scenarios"
MODEL="${JOULIE_SKILL_TEST_MODEL:-sonnet}"
BASELINE=0
NAMES=()

while [ $# -gt 0 ]; do
  case "$1" in
    --baseline) BASELINE=1 ;;
    --model) MODEL="$2"; shift ;;
    -h|--help) sed -n '2,6p' "$0"; exit 0 ;;
    *) NAMES+=("$1") ;;
  esac
  shift
done

command -v claude >/dev/null || { echo "claude CLI not found" >&2; exit 2; }

WORKDIR="$REPO"
if [ "$BASELINE" = 1 ]; then
  # Same repository, skill hidden, so the only difference is the skill.
  WORKDIR="$(mktemp -d)"
  rsync -a --exclude '.claude/skills/joulie-dev' --exclude '.git' "$REPO/" "$WORKDIR/"
  trap 'rm -rf "$WORKDIR"' EXIT
fi

RESULTS="${JOULIE_SKILL_TEST_RESULTS:-$(mktemp -d)}"
mkdir -p "$RESULTS"

if [ ${#NAMES[@]} -eq 0 ]; then
  for f in "$SCENARIOS"/*.md; do NAMES+=("$(basename "$f" .md)"); done
fi

# section FILE NAME: print the body of "## NAME" up to the next "## ".
section() {
  awk -v want="## $2" '
    $0 == want {p=1; next}
    /^## / {p=0}
    p {print}
  ' "$1"
}

failed=0
for name in "${NAMES[@]}"; do
  file="$SCENARIOS/$name.md"
  [ -f "$file" ] || { echo "no scenario $name" >&2; failed=$((failed+1)); continue; }
  prompt="$(section "$file" Prompt)"
  answer="$RESULTS/$name.$([ "$BASELINE" = 1 ] && echo baseline || echo skill).txt"

  # Read-only tool set: the prompts ask for plans and diagnoses, never edits.
  (cd "$WORKDIR" && claude -p "$prompt" --model "$MODEL" --output-format text \
      --allowedTools "Read,Grep,Glob,Bash(ls:*),Bash(cat:*),Bash(grep:*),Bash(find:*),Bash(go list:*)" \
      > "$answer" 2>/dev/null) || true

  missed=()
  while IFS= read -r expect; do
    expect="${expect#- }"
    [ -z "$expect" ] && continue
    grep -qiE -- "$expect" "$answer" || missed+=("$expect")
  done < <(section "$file" Expect | grep '^- ')

  if [ ${#missed[@]} -eq 0 ]; then
    echo "PASS $name"
  else
    failed=$((failed+1))
    echo "FAIL $name ($answer)"
    for m in "${missed[@]}"; do echo "     missing: $m"; done
  fi
done

echo "results in $RESULTS"
exit "$failed"

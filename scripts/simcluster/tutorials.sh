#!/usr/bin/env bash
#
# tutorials.sh - run tutorials under examples/ as a reader would, and check
# what they say happens.
#
# Usage: scripts/simcluster/tutorials.sh [--print] DIR...
#
# Runs the ```bash blocks of each DIR/README.md in document order, from the
# repository root, in one bash with -euo pipefail, so a tutorial that exports
# KUBECONFIG serves the ones after it. Two HTML comments, which GitHub does not
# show, steer it:
#   <!-- tutorials: skip -->  on the line before a bash block: do not run it
#                             (an interactive or very long command, a cleanup
#                             a later tutorial would miss)
#   <!-- tutorials: check     a check, run where it stands: the shell lines up
#   ...                       to the closing --> line; a non-zero status fails
#   -->                       the run
# A check can call eventually 'EXPR', which evaluates EXPR every 2 s until it
# succeeds, for up to TUTORIALS_TIMEOUT seconds (default 180).
# --print prints the assembled script instead of running it. Exits with the
# status of the first command or check that fails.
#
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
print=0
case ${1:-} in
  -h | --help)
    awk 'NR > 2 && /^#/ { sub(/^# ?/, ""); print; next } NR > 2 { exit }' "$0"
    exit 0
    ;;
  --print)
    print=1
    shift
    ;;
esac
(($#)) || { echo "tutorials.sh: name at least one tutorial directory (see --help)" >&2; exit 2; }

script=$(mktemp)
trap 'rm -f "$script"' EXIT
{
  cat <<'EOF'
set -euo pipefail
eventually() {
  local end=$((SECONDS + ${TUTORIALS_TIMEOUT:-180}))
  until eval "$1"; do
    if ((SECONDS >= end)); then
      echo "tutorials.sh: still false after ${TUTORIALS_TIMEOUT:-180} s: $1" >&2
      return 1
    fi
    sleep 2
  done
}
EOF
  for dir in "$@"; do
    dir=${dir%/}
    readme=$repo/$dir/README.md
    [[ -f $readme ]] || { echo "tutorials.sh: $readme: no such file" >&2; exit 2; }
    awk -v dir="$dir" '
      function header(what) { printf "echo %c==> %s:%d %s%c >&2\n", 39, dir "/README.md", NR, what, 39 }
      /^<!-- tutorials: skip -->[[:space:]]*$/ { skip = 1; next }
      /^<!-- tutorials: check[[:space:]]*$/ { check = 1; header("check"); next }
      check && /^-->[[:space:]]*$/ { check = 0; next }
      check { print; next }
      /^```bash[[:space:]]*$/ { block = 1; drop = skip; skip = 0; header(drop ? "skipped" : "run"); next }
      block && /^```[[:space:]]*$/ { block = 0; next }
      block && !drop { print }
      END {
        if (block || check) { printf "tutorials.sh: %s/README.md: a block or check is not closed\n", dir > "/dev/stderr"; exit 1 }
      }
    ' "$readme"
  done
} >"$script"

if ((print)); then
  cat "$script"
  exit 0
fi
cd "$repo"
bash "$script"

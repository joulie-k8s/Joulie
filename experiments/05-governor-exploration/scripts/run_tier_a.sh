#!/usr/bin/env bash
#
# run_tier_a.sh - Tier A of experiment 05: the offline governor sweep on
# emulated nodes, no Kubernetes, in virtual time.
#
# Usage: experiments/05-governor-exploration/scripts/run_tier_a.sh [config] [out]
#
#   config  a govsweep config (default configs/tier-a.yaml of this experiment)
#   out     the run directory (default
#           ${XDG_CACHE_HOME:-~/.cache}/joulie/experiments/05/<UTC>-<config>)
#
# Builds simulator/cmd/govsweep from the working tree, runs the cells of the
# config 8 at a time, since the host is shared (GOVSWEEP_PARALLEL to change
# it), with the node trees in /dev/shm when it exists (GOVSWEEP_WORK to
# change it), then summarises the CSVs with analysis/summarize.py into
# summary.md. The run directory gets the CSVs, the OTLP JSON Lines,
# govsweep.log, the summary and the code it ran (code.txt, code.diff);
# nothing is written into the repository. SUMMARY_ONLY=1 with an existing
# run directory skips the sweep and summarises its CSVs again, adding the
# summarize.py it used to code.txt.
#
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
exp=$(dirname "$here")
repo=$(cd "$exp/../.." && pwd)
case ${1:-} in
  -h | --help)
    awk 'NR > 2 && /^#/ { sub(/^# ?/, ""); print; next } NR > 2 { exit }' "$0"
    exit 0
    ;;
esac
config=${1:-$exp/configs/tier-a.yaml}
[[ -f $config ]] || { echo "run_tier_a.sh: $config: no such file" >&2; exit 2; }
config=$(cd "$(dirname "$config")" && pwd)/$(basename "$config")
name=$(basename "$config" .yaml)
out=${2:-${XDG_CACHE_HOME:-$HOME/.cache}/joulie/experiments/05/$(date -u +%Y%m%dT%H%M%SZ)-$name}
mkdir -p "$out"
out=$(cd "$out" && pwd)
case $out/ in
  "$repo"/*) echo "run_tier_a.sh: $out is inside the repository; results stay outside it" >&2; exit 2 ;;
esac

summarise() {
  echo "summarize $(sha256sum <"$exp/analysis/summarize.py" | cut -d' ' -f1)" >>"$out/code.txt"
  python3 "$exp/analysis/summarize.py" "$out" >"$out/summary.md"
}
if [[ ${SUMMARY_ONLY:-} == 1 ]]; then
  [[ -f $out/code.txt ]] || { echo "run_tier_a.sh: $out holds no run to summarise" >&2; exit 2; }
  summarise
  echo "run_tier_a.sh: summary in $out/summary.md" >&2
  exit 0
fi

export PATH="$HOME/.local/go/bin:$PATH"
GO=${GO:-$(command -v go)}
bin=$out/bin
mkdir -p "$bin"
# The code this run builds from: govsweep's own build information names the
# revision of the repository's main checkout, not a worktree's changes.
{
  echo "head $(git -C "$repo" rev-parse HEAD)"
  echo "diff $(git -C "$repo" diff HEAD | sha256sum | cut -d' ' -f1)"
  echo "untracked $(git -C "$repo" ls-files --others --exclude-standard -z | (cd "$repo" && xargs -0 -r sha256sum) | sha256sum | cut -d' ' -f1)"
  echo "config $(sha256sum <"$config" | cut -d' ' -f1)"
} >"$out/code.txt"
git -C "$repo" diff HEAD >"$out/code.diff"
cp "$config" "$out/"
(cd "$repo" && "$GO" build -trimpath -o "$bin/govsweep" ./simulator/cmd/govsweep)

work_base=${GOVSWEEP_WORK:-}
if [[ -z $work_base ]]; then
  if [[ -d /dev/shm && -w /dev/shm ]]; then work_base=/dev/shm; else work_base=${TMPDIR:-/tmp}; fi
fi
work=$(mktemp -d "$work_base/govsweep-$name.XXXXXX")
trap 'rm -rf "$work"' EXIT

parallel=${GOVSWEEP_PARALLEL:-8}
echo "run_tier_a.sh: $config -> $out ($parallel at a time, trees in $work)" >&2
status=0
"$bin/govsweep" -config "$config" -out "$out" -work "$work" -parallel "$parallel" 2>&1 | tee "$out/govsweep.log" || status=$?
if command -v python3 >/dev/null 2>&1; then
  summarise || status=1
  echo "run_tier_a.sh: summary in $out/summary.md" >&2
fi
exit "$status"

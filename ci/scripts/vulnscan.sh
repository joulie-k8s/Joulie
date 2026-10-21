#!/usr/bin/env bash
#
# vulnscan.sh - scan the dependencies for known vulnerabilities.
#
# Usage: ci/scripts/vulnscan.sh <out dir>
#
# Runs at the repository root, inside the vulnscan container of the Dagger
# module in ci/. osv-scanner reads every manifest of $VULN_MANIFESTS, with
# call analysis for Go, so an advisory whose code the module never calls is
# reported but does not fail; govulncheck checks each Go module of
# $GOVULNCHECK_MODULES for vulnerabilities its code reaches, the standard
# library's included. An osv-scanner.toml at the root, when present, ignores
# the advisories it lists, in both.
#
# Writes into <out dir>: osv.json and osv.sarif, govulncheck-<module>.json
# and .sarif (root for the root module, the path with dashes otherwise), the
# SARIF paths relative to the repository; summary.txt, one line per finding;
# and status: 0 for no finding, 1 for findings. Exits 2 when a scanner
# fails (osv-scanner exits 0 clean, 1 with findings, anything else on an
# error), otherwise 0, findings or not.
set -uo pipefail

out=${1:?usage: vulnscan.sh <out dir>}
mkdir -p "$out"
root=$PWD

fail() {
  printf 'vulnscan.sh: %s\n' "$*" >&2
  exit 2
}

# relativize rewrites the repository's absolute paths in a SARIF file to
# paths relative to its root, as code scanning matches them to files.
relativize() {
  sed -i -e "s#file://$root/##g" -e "s#\"$root/#\"#g" "$1"
}

config=()
ignored=()
if [[ -f osv-scanner.toml ]]; then
  config=(--config osv-scanner.toml)
  # The ids of the [[IgnoredVulns]] entries whose ignoreUntil, if any, has
  # not passed, as osv-scanner reads them.
  mapfile -t ignored < <(awk -v today="$(date -u +%Y-%m-%d)" '
    function flush() { if (id != "" && (until == "" || until >= today)) print id; id = ""; until = "" }
    /^[[:space:]]*\[\[IgnoredVulns\]\]/ { flush(); next }
    /^[[:space:]]*id[[:space:]]*=/ { if (match($0, /"[^"]*"/)) id = substr($0, RSTART + 1, RLENGTH - 2) }
    /^[[:space:]]*ignoreUntil[[:space:]]*=/ { v = $0; sub(/^[^=]*=[[:space:]]*/, "", v); gsub(/"/, "", v); until = substr(v, 1, 10) }
    END { flush() }' osv-scanner.toml)
fi
manifests=()
for m in $VULN_MANIFESTS; do
  [[ -f $m ]] || fail "manifest $m does not exist"
  manifests+=(-L "$m")
done

status=0
lines=()

osv-scanner scan source "${config[@]}" "${manifests[@]}" --format json --output-file "$out/osv.json"
rc=$?
case $rc in
0) ;;
1) status=1 ;;
*) fail "osv-scanner exited $rc" ;;
esac
osv-scanner scan source "${config[@]}" "${manifests[@]}" --format sarif --output-file "$out/osv.sarif"
rc=$?
((rc <= 1)) || fail "osv-scanner exited $rc writing SARIF"
relativize "$out/osv.sarif"

# One line per advisory group: the manifest, the package and its version,
# the advisories, and whether the module calls the vulnerable code.
while IFS= read -r line; do
  lines+=("$line")
done < <(jq -r --arg root "$root/" '
  .results[]? | (.source.path | ltrimstr($root)) as $path
  | .packages[]? | .package as $pkg | .groups[]?
  | ((.experimental_analysis // {}) | [.[]?.called] | if length == 0 then true else any end) as $called
  | "osv-scanner  \($path)  \($pkg.name)@\($pkg.version)  \(.ids | join(","))  \(if $called then "called" else "not called, does not fail" end)"' "$out/osv.json")

for m in $GOVULNCHECK_MODULES; do
  name=root
  [[ $m == . ]] || name=${m//\//-}
  (cd "$m" && govulncheck -format json ./...) >"$out/govulncheck-$name.json" || fail "govulncheck failed in $m"
  (cd "$m" && govulncheck -format sarif ./...) >"$out/govulncheck-$name.sarif" || fail "govulncheck failed in $m writing SARIF"
  relativize "$out/govulncheck-$name.sarif"
  # A finding whose trace starts at a function is one the code reaches.
  while IFS=$'\t' read -r id module version fixed; do
    [[ -n $id ]] || continue
    if printf '%s\n' "${ignored[@]}" | grep -qxF "$id"; then
      lines+=("govulncheck  $m  $module@$version  $id  ignored in osv-scanner.toml")
      continue
    fi
    status=1
    what="reachable, fixed in ${fixed:-no release}"
    [[ $module == stdlib ]] && what="$what, needs a toolchain bump"
    lines+=("govulncheck  $m  $module@$version  $id  $what")
  done < <(jq -rs '[.[] | .finding? // empty | select(.trace[0].function != null)
    | [.osv, .trace[0].module, (.trace[0].version // ""), (.fixed_version // "")]] | unique[] | @tsv' "$out/govulncheck-$name.json")
done

{
  if ((status == 0)); then
    echo "PASS vulnscan: no vulnerability the code reaches"
  else
    echo "FAIL vulnscan: vulnerabilities the code reaches"
  fi
  printf '%s\n' "${lines[@]}"
} >"$out/summary.txt"
echo "$status" >"$out/status"
cat "$out/summary.txt"

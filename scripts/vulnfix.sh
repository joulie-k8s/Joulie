#!/usr/bin/env bash
#
# vulnfix.sh - apply the fixes an osv-scanner JSON report names.
#
# Usage: scripts/vulnfix.sh <osv-scanner json> <report.md> [<module dir>=<govulncheck json>]...
#
# Runs at the repository root; the nightly vulnerability scan runs it
# through the vulnfix function of the Dagger module in ci/. Each finding is
# a package of a manifest (the source path of a result, absolute under the
# root or relative to it) with its advisories, each advisory's fixed
# versions taken from the ranges of the affected entry of that package. An
# advisory listed twice counts once, with its highest fix, and a package
# goes to the highest fix of its advisories.
#
# - A Go module (go.mod): go get <module>@v<fix> in the manifest's
#   directory, then go mod tidy; when either fails, go.mod and go.sum go back
#   to what they were before that package and it is listed as not fixed.
# - stdlib: never applied; listed as needing a toolchain bump (the go.mod
#   toolchain line and the golang image digests), with the standard library
#   findings the code reaches in each govulncheck JSON given after the
#   report, of the Go module in <module dir>.
# - A Python package of a requirements file: the version of a plain
#   name==version line is replaced; any other requirement is listed.
# - A package with no fixed version is listed.
#
# Writes report.md, the tables of what was bumped and what was not, with a
# link to each advisory. Exits 0 whatever it fixed; 1 when the report cannot
# be read.
set -euo pipefail

usage="usage: vulnfix.sh <osv-scanner json> <report.md> [<module dir>=<govulncheck json>]..."
scan=${1:?$usage}
report=${2:?$usage}
shift 2
[[ -f $scan ]] || {
  echo "vulnfix.sh: no report $scan" >&2
  exit 1
}
root=$PWD

# One line per package and advisory: path, ecosystem, name, version,
# advisory and its highest fix (empty when it has none), tab separated.
findings=$(jq -r --arg root "$root/" '
  .results[]? | (.source.path | ltrimstr($root)) as $path
  | .packages[]? | .package as $pkg
  | .vulnerabilities[]? | .id as $id
  | [.affected[]? | select(.package.name == $pkg.name) | .ranges[]?.events[]?.fixed // empty] as $fixed
  | [$path, $pkg.ecosystem, $pkg.name, $pkg.version, $id, ($fixed | join(" "))] | @tsv' "$scan") || {
  echo "vulnfix.sh: $scan is not an osv-scanner JSON report" >&2
  exit 1
}

# The standard library findings govulncheck says the code reaches, as more
# lines of the same shape, the manifest being the module's go.mod.
for arg in "$@"; do
  dir=${arg%%=*} file=${arg#*=}
  [[ $arg == *=* && -f $file ]] || {
    echo "vulnfix.sh: $arg is not <module dir>=<govulncheck json>" >&2
    exit 1
  }
  more=$(jq -rs --arg path "${dir%/}/go.mod" '[.[] | .finding? // empty
    | select(.trace[0].function != null and .trace[0].module == "stdlib")
    | [($path | ltrimstr("./")), "Go", "stdlib", (.trace[0].version | ltrimstr("v")), .osv, ((.fixed_version // "") | ltrimstr("v"))]]
    | unique[] | @tsv' "$file") || {
    echo "vulnfix.sh: $file is not a govulncheck JSON stream" >&2
    exit 1
  }
  [[ -z $more ]] || findings+=$'\n'"$more"
done

# highest prints the highest of its version arguments.
highest() {
  printf '%s\n' "$@" | sort -V | tail -n 1
}

# The packages in the order first seen, their versions, advisories and fixes.
declare -A version advisories fix unfixed
order=()
while IFS=$'\t' read -r path eco name ver id fixes; do
  [[ -n $path ]] || continue
  key="$path"$'\t'"$eco"$'\t'"$name"
  if [[ -z ${version[$key]+set} ]]; then
    order+=("$key")
    version[$key]=$ver
    advisories[$key]=""
    fix[$key]=""
    unfixed[$key]=""
  fi
  case " ${advisories[$key]} " in
  *" $id "*) ;;
  *) advisories[$key]="${advisories[$key]:+${advisories[$key]} }$id" ;;
  esac
  if [[ -n $fixes ]]; then
    # shellcheck disable=SC2086 # the fixes are a space separated list
    best=$(highest $fixes ${fix[$key]})
    fix[$key]=$best
  else
    unfixed[$key]="${unfixed[$key]:+${unfixed[$key]} }$id"
  fi
done <<<"$findings"

links() {
  local id out=()
  for id in $1; do
    out+=("[$id](https://osv.dev/vulnerability/$id)")
  done
  local IFS=,
  printf '%s' "${out[*]}" | sed 's/,/, /g'
}

bumped=() toolchain=() notfixed=()

for key in "${order[@]}"; do
  IFS=$'\t' read -r path eco name <<<"$key"
  ver=${version[$key]} to=${fix[$key]} ids=${advisories[$key]}
  if [[ -z $to ]]; then
    notfixed+=("| \`$path\` | \`$name\` | $ver | $(links "$ids") | no fixed version |")
    continue
  fi
  if [[ -n ${unfixed[$key]} ]]; then
    notfixed+=("| \`$path\` | \`$name\` | $ver | $(links "${unfixed[$key]}") | no fixed version; the others are fixed in $to |")
  fi
  case "$eco/$name" in
  Go/stdlib)
    toolchain+=("| \`$path\` | $ver | $to | $(links "$ids") |")
    ;;
  Go/*)
    dir=$(dirname "$path")
    cp "$dir/go.mod" "$dir/go.mod.vulnfix"
    [[ -f $dir/go.sum ]] && cp "$dir/go.sum" "$dir/go.sum.vulnfix"
    if (cd "$dir" && go get "$name@v$to" && go mod tidy) >&2; then
      bumped+=("| \`$path\` | \`$name\` | $ver | $to | $(links "$ids") |")
      rm -f "$dir/go.mod.vulnfix" "$dir/go.sum.vulnfix"
    else
      mv "$dir/go.mod.vulnfix" "$dir/go.mod"
      [[ -f $dir/go.sum.vulnfix ]] && mv "$dir/go.sum.vulnfix" "$dir/go.sum"
      notfixed+=("| \`$path\` | \`$name\` | $ver | $(links "$ids") | \`go get $name@v$to\` or \`go mod tidy\` failed; reverted |")
    fi
    ;;
  PyPI/*)
    # A plain pin: the name, as written in any case, == the version.
    name_re=${name//[-_.]/[-_.]}
    ver_re=${ver//./\\.}
    pin="^([[:space:]]*)($name_re)([[:space:]]*==[[:space:]]*)$ver_re([[:space:]]*(#.*)?)$"
    if grep -qiE "$pin" "$path"; then
      sed -i -E "s/$pin/\\1\\2\\3$to\\4/I" "$path"
      bumped+=("| \`$path\` | \`$name\` | $ver | $to | $(links "$ids") |")
    else
      notfixed+=("| \`$path\` | \`$name\` | $ver | $(links "$ids") | not a plain \`$name==$ver\` line; fixed in $to |")
    fi
    ;;
  *)
    notfixed+=("| \`$path\` | \`$name\` | $ver | $(links "$ids") | $eco is not fixed automatically; fixed in $to |")
    ;;
  esac
done

{
  echo "Fixes from the nightly vulnerability scan (osv-scanner and govulncheck)."
  if ((${#bumped[@]})); then
    printf '\n## Bumped\n\n| Manifest | Package | From | To | Advisories |\n|---|---|---|---|---|\n'
    printf '%s\n' "${bumped[@]}"
  fi
  if ((${#toolchain[@]})); then
    # shellcheck disable=SC2016 # the backticks are Markdown
    printf '\n## Needs a toolchain bump\n\nThe standard library is fixed by a Go release: bump the `toolchain` line of `go.mod` and the golang image digests (`Dockerfile`, `simulator/Dockerfile`, `ci/src/joulie_ci/literals.py`).\n\n| Manifest | Go | Fixed in | Advisories |\n|---|---|---|---|\n'
    printf '%s\n' "${toolchain[@]}"
  fi
  if ((${#notfixed[@]})); then
    printf '\n## Not fixed\n\n| Manifest | Package | Version | Advisories | Why |\n|---|---|---|---|---|\n'
    printf '%s\n' "${notfixed[@]}"
  fi
  if ((${#order[@]} == 0)); then
    printf '\nNo finding.\n'
  fi
} >"$report"
printf 'vulnfix.sh: %d bumped, %d need a toolchain bump, %d not fixed; report in %s\n' \
  "${#bumped[@]}" "${#toolchain[@]}" "${#notfixed[@]}" "$report" >&2

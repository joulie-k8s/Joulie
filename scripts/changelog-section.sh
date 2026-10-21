#!/usr/bin/env bash
#
# changelog-section.sh prints the release notes of one version: the body of
# its "## <tag> ..." section in CHANGELOG.md, without the heading, up to the
# next "## " heading. The release workflow publishes it as the GitHub release
# notes when a release has none.
#
# Usage: scripts/changelog-section.sh <tag> [changelog]
#
# Exits 1 when the changelog has no section for the tag or the section is
# empty, so a release is never given empty notes.
set -euo pipefail
if [ $# -lt 1 ] || [ $# -gt 2 ]; then
	echo "usage: changelog-section.sh <tag> [changelog]" >&2
	exit 2
fi
tag=$1
file=${2:-$(dirname "$0")/../CHANGELOG.md}
body=$(awk -v tag="$tag" '
	/^## / { if (in_section) exit; in_section = ($2 == tag) ; next }
	in_section { print }
' "$file" | sed -e '/./,$!d' | sed -e ':a' -e '/^\n*$/{$d;N;ba' -e '}')
if [ -z "$body" ]; then
	echo "changelog-section.sh: $file has no notes for $tag" >&2
	exit 1
fi
printf '%s\n' "$body"

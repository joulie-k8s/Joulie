#!/usr/bin/env bash
#
# release-version.sh turns a release tag into the version a release publishes
# and decides whether that release may move the ":latest" image tags.
#
# ":latest" names the newest stable release. A pre-release must not move it,
# whether GitHub flags it as one or it is only named like one: v0.2.0-rc0 was
# first published without the flag, and every ":latest" pointed at it.
#
# Usage: scripts/release-version.sh <tag> <prerelease: true|false>
#
# Prints "version=<tag without the leading v>" and "latest=<true|false>", the
# format the release workflow appends to $GITHUB_OUTPUT.

set -euo pipefail

if [ $# -ne 2 ]; then
	echo "usage: release-version.sh <tag> <prerelease: true|false>" >&2
	exit 2
fi

tag="$1"
prerelease="$2"
version="${tag#v}"

latest=true
# A semver pre-release carries a hyphen after the patch number: 0.2.0-rc0.
if [ "${prerelease}" = "true" ] || [[ "${version}" == *-* ]]; then
	latest=false
fi

echo "version=${version}"
echo "latest=${latest}"

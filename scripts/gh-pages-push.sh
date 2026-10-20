#!/usr/bin/env bash
#
# gh-pages-push.sh - change the gh-pages branch and push, trying again when
# another workflow pushed first.
#
# Usage: scripts/gh-pages-push.sh publish <site dir> <destination dir> <message>
#        scripts/gh-pages-push.sh remove <destination dir> <message>
#        scripts/gh-pages-push.sh push
#
# Every docs workflow writes gh-pages: each pull request's preview, the main
# docs, a release, a preview's cleanup. Their concurrency groups differ, so
# that a preview never evicts a main or release deploy from a queue, and two
# of them can push at once: the later push is rejected, as two previews were
# when eight pull requests opened together.
#
# publish clones gh-pages (depth 1) into a temporary directory, copies the
# site over <destination dir> without deleting what is already there, and
# commits; remove deletes <destination dir> and commits; push takes the
# commits already made in the gh-pages clone it runs in. Then each pushes,
# and a rejected push fetches gh-pages, rebases the commits onto it and
# tries again, GH_PAGES_TRIES times in all (5), waiting GH_PAGES_RETRY_SECONDS
# (5) times the try number plus up to as many seconds of jitter. Nothing to
# commit is no push. A rebase that conflicts fails at once.
#
# The remote is GH_PAGES_REMOTE, by default
# https://x-access-token:$GITHUB_TOKEN@github.com/$GITHUB_REPOSITORY.git;
# commits are made as github-actions[bot].
#
set -euo pipefail

die() {
  printf 'gh-pages-push.sh: %s\n' "$*" >&2
  exit 1
}

tries=${GH_PAGES_TRIES:-5}
delay=${GH_PAGES_RETRY_SECONDS:-5}
branch=gh-pages

remote() {
  if [[ -n ${GH_PAGES_REMOTE:-} ]]; then
    printf '%s\n' "$GH_PAGES_REMOTE"
    return
  fi
  [[ -n ${GITHUB_TOKEN:-} && -n ${GITHUB_REPOSITORY:-} ]] ||
    die "set GH_PAGES_REMOTE, or GITHUB_TOKEN and GITHUB_REPOSITORY"
  printf 'https://x-access-token:%s@github.com/%s.git\n' "$GITHUB_TOKEN" "$GITHUB_REPOSITORY"
}

# clone checks out gh-pages, depth 1, into a new directory and prints it.
clone() {
  local dir
  dir=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/gh-pages.XXXXXX")
  git clone --quiet --depth 1 --branch "$branch" "$(remote)" "$dir" >&2
  git -C "$dir" config user.name "github-actions[bot]"
  git -C "$dir" config user.email "41898282+github-actions[bot]@users.noreply.github.com"
  printf '%s\n' "$dir"
}

# commit_all commits everything in the clone dir as message; it succeeds
# with nothing to commit, and then makes no commit.
commit_all() {
  local dir=$1 message=$2
  git -C "$dir" add -A
  if git -C "$dir" diff --cached --quiet; then
    printf 'gh-pages-push.sh: nothing to commit\n' >&2
    return 0
  fi
  git -C "$dir" commit --quiet -m "$message"
}

# push_with_retry pushes the commits of the clone dir that gh-pages lacks.
push_with_retry() {
  local dir=$1 url try wait
  url=$(git -C "$dir" remote get-url origin)
  git -C "$dir" fetch --quiet "$url" "$branch"
  if [[ -z $(git -C "$dir" rev-list FETCH_HEAD..HEAD) ]]; then
    printf 'gh-pages-push.sh: %s already has every commit\n' "$branch" >&2
    return 0
  fi
  for ((try = 1; try <= tries; try++)); do
    if git -C "$dir" push --quiet "$url" "HEAD:refs/heads/$branch"; then
      printf 'gh-pages-push.sh: pushed %s (try %d of %d)\n' "$branch" "$try" "$tries" >&2
      return 0
    fi
    ((try < tries)) || break
    wait=$((delay * try + RANDOM % (delay * try + 1)))
    printf 'gh-pages-push.sh: the push was rejected (try %d of %d); rebasing onto %s in %ds\n' "$try" "$tries" "$branch" "$wait" >&2
    sleep "$wait"
    git -C "$dir" fetch --quiet "$url" "$branch"
    if ! git -C "$dir" rebase --quiet FETCH_HEAD >&2; then
      git -C "$dir" rebase --abort || true
      die "the commits do not rebase onto the new $branch"
    fi
  done
  die "the push was rejected $tries times"
}

case ${1:-} in
publish)
  (($# == 4)) || die "usage: publish <site dir> <destination dir> <message>"
  site=$2 dest=${3%/} message=$4
  [[ -d $site ]] || die "no site directory $site"
  [[ -n $dest && $dest != /* && $dest != *..* ]] || die "the destination must be a relative directory: $dest"
  dir=$(clone)
  trap 'rm -rf "$dir"' EXIT
  mkdir -p "$dir/$dest"
  cp -a "$site/." "$dir/$dest/"
  commit_all "$dir" "$message"
  push_with_retry "$dir"
  ;;
remove)
  (($# == 3)) || die "usage: remove <destination dir> <message>"
  dest=${2%/} message=$3
  [[ -n $dest && $dest != /* && $dest != *..* ]] || die "the destination must be a relative directory: $dest"
  dir=$(clone)
  trap 'rm -rf "$dir"' EXIT
  rm -rf "${dir:?}/$dest"
  commit_all "$dir" "$message"
  push_with_retry "$dir"
  ;;
push)
  (($# == 1)) || die "usage: push"
  git rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "push runs inside a clone of $branch"
  push_with_retry "$PWD"
  ;;
-h | --help)
  sed -n '3,27p' "$0" | sed 's/^# \{0,1\}//'
  ;;
*) die "unknown command ${1:-} (see --help)" ;;
esac

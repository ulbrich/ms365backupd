#!/usr/bin/env bash
# Releases the next version. Run it as `make release` from a clean main that's
# in sync with origin (merge develop into main first).
#
#   script/release.sh               release the next version
#   script/release.sh --next        only print the next tag, the last one and
#                                   the latest published GitHub release
#
#   VERSION=1.2.3   use this version instead of the latest tag's patch + 1
#
# A release is a vx.y.z tag on main. Pushing it starts the release workflow
# (.github/workflows/release.yml), which runs the tests, builds the binaries
# (make dist) and publishes them as the GitHub release vx.y.z:
#
#   1. tag the current main as vx.y.z and push only the tag
#   2. wait for the release workflow of that tag to succeed
#   3. check that the GitHub release exists with its binaries
#
# If it stops after step 1, rerun with VERSION=x.y.z: an existing tag on
# main's history is reused instead of tagged again.

set -euo pipefail

NAME=ms365backupd
BRANCH=main
REPO_DIR=$(cd "$(dirname "$0")/.." && pwd)
WORKFLOW=release.yml
PREFIX=v

die() { echo "release: $*" >&2; exit 1; }

mode=release
case "${1:-}" in
  --next) mode=next; shift ;;
  -*) die "usage: $0 [--next]" ;;
esac

cd "$REPO_DIR"

# The latest published GitHub release (e.g. v1.2.6), empty if none/offline.
published() {
  gh release view --json tagName -q .tagName 2>/dev/null || true
}

# Checks that we're on a clean branch in sync with origin.
check_clean() {
  [ "$(git symbolic-ref --short -q HEAD || true)" = "$BRANCH" ] || die "releases are made from $BRANCH, not $(git symbolic-ref --short -q HEAD || git rev-parse --short HEAD)"
  git diff --quiet HEAD -- || die "there are uncommitted changes; commit or stash them first"
  [ -z "$(git ls-files --others --exclude-standard)" ] || die "there are untracked files; commit or remove them first"
  git fetch --quiet origin "$BRANCH" --tags
  [ "$(git rev-parse HEAD)" = "$(git rev-parse "origin/$BRANCH")" ] || die "$BRANCH isn't the same as origin/$BRANCH; push (or pull) first"
}

# The latest vx.y.z tag, sorted numerically (1.1.10 after 1.1.9). Other tags
# (e.g. v2.2.0-rc1) are ignored.
last=$(git tag -l "$PREFIX[0-9]*.[0-9]*.[0-9]*" | sed "s/^$PREFIX//" |
  grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -t. -k1,1n -k2,2n -k3,3n | tail -1 || true)
if [ -n "${VERSION:-}" ]; then
  version=$VERSION
elif [ -n "$last" ]; then
  version=$(echo "$last" | awk -F. '{ printf "%d.%d.%d", $1, $2, $3 + 1 }')
else
  version=1.0.0
fi
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "VERSION must look like 1.2.3, not '$version'"
tag="$PREFIX$version"

if [ "$mode" = next ]; then
  now=$(published)
  echo "$tag ${last:+$PREFIX}${last:-none} ${now:-none}"
  exit 0
fi

# --- Checks ------------------------------------------------------------------

command -v gh >/dev/null || die "gh not found; it waits for the release build"
check_clean

# --- 1. Tag ------------------------------------------------------------------

if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
  # Resuming an interrupted release: only reuse a tag that's already on this
  # branch, never one pointing somewhere else.
  git merge-base --is-ancestor "$tag^{commit}" HEAD ||
    die "tag $tag exists but isn't on $BRANCH; pick another VERSION"
  echo "release: reusing existing tag $tag"
  git ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null ||
    git push --quiet origin "$tag" || die "pushing $tag failed"
else
  echo "release: tagging and pushing $tag"
  git tag -a "$tag" -m "$NAME $version"
  git push --quiet origin "$tag" ||
    die "pushing $tag failed; fix it and rerun (or undo with: git tag -d $tag)"
fi

# --- 2. Wait for the release build -------------------------------------------

echo "release: waiting for the GitHub Actions release build of $tag"
run=
for _ in $(seq 30); do
  run=$(gh run list --workflow "$WORKFLOW" --limit 20 --json databaseId,headBranch \
    -q ".[] | select(.headBranch == \"$tag\") | .databaseId" 2>/dev/null | head -1)
  [ -n "$run" ] && break
  sleep 5
done
[ -n "$run" ] || die "no GitHub Actions run for $tag showed up; check with: gh run list --workflow $WORKFLOW"
gh run watch "$run" --exit-status --interval 10 >/dev/null ||
  die "the release build of $tag failed; see: gh run view $run --log-failed (rerun with VERSION=$version once fixed)"

# --- 3. Check the release ----------------------------------------------------

assets=$(gh release view "$tag" --json assets -q '.assets | length' 2>/dev/null || echo 0)
[ "$assets" -gt 0 ] || die "the build succeeded but release $tag has no binaries; check: gh release view $tag"
url=$(gh release view "$tag" --json url -q .url)
echo "release: done, $tag is published with $assets files: $url"

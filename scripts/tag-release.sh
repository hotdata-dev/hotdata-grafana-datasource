#!/usr/bin/env bash
# Tag and publish a release.
#
# Flow: merge a PR to main that bumps the version in package.json (and adds a
# CHANGELOG entry), then run this script. It tags v<version> at origin/main,
# which triggers the release workflow; the script waits for the build and
# publishes the draft release.
#
# Usage:
#   ./scripts/tag-release.sh              # tag, wait for build, publish
#   ./scripts/tag-release.sh --no-publish # tag only; leave the release a draft
set -euo pipefail

REPO="hotdata-dev/hotdata-grafana-datasource"
PLUGIN_ID="hotdata-sql-datasource"
PUBLISH=true
case "${1:-}" in
  '') ;;
  --no-publish) PUBLISH=false ;;
  *) echo "error: unknown argument '${1}' (only --no-publish is supported)" >&2; exit 1 ;;
esac

command -v gh >/dev/null || { echo "error: gh CLI is required" >&2; exit 1; }

git fetch origin --tags

VERSION=$(git show origin/main:package.json | sed -n 's/.*"version": "\([0-9]*\.[0-9]*\.[0-9]*\)".*/\1/p' | head -1)
[ -n "$VERSION" ] || { echo "error: could not read version from package.json on origin/main" >&2; exit 1; }
TAG="v$VERSION"

if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null || git ls-remote --exit-code --tags origin "$TAG" >/dev/null 2>&1; then
  echo "error: tag $TAG already exists — bump the version in package.json (via PR) first" >&2
  exit 1
fi

if ! git show origin/main:CHANGELOG.md | grep -q "^## $VERSION"; then
  echo "warning: CHANGELOG.md on origin/main has no '## $VERSION' entry" >&2
fi

echo "Tagging $TAG at $(git rev-parse --short origin/main) ..."
git tag "$TAG" origin/main
git push origin "$TAG"

echo "Waiting for the release workflow ..."
RUN_ID=""
for _ in $(seq 1 24); do
  RUN_ID=$(gh run list --repo "$REPO" --workflow release.yml --branch "$TAG" --limit 1 --json databaseId --jq '.[0].databaseId // empty')
  [ -n "$RUN_ID" ] && break
  sleep 5
done
[ -n "$RUN_ID" ] || { echo "error: no release workflow run appeared for $TAG after 2 minutes" >&2; exit 1; }
gh run watch "$RUN_ID" --repo "$REPO" --exit-status >/dev/null || {
  echo "error: release workflow failed — see gh run view $RUN_ID --repo $REPO" >&2
  exit 1
}

ZIP_URL="https://github.com/$REPO/releases/download/$TAG/$PLUGIN_ID-$VERSION.zip"
if $PUBLISH; then
  gh release edit "$TAG" --repo "$REPO" --draft=false >/dev/null
  echo "Published $TAG."
else
  echo "Release $TAG left as a draft (publish with: gh release edit $TAG --repo $REPO --draft=false)."
fi

echo "zip:  $ZIP_URL"
echo "sha1: $(curl -sfL "$ZIP_URL.sha1" || echo '<publish the release to fetch>')"

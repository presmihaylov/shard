#!/usr/bin/env bash
# Tags one SDK version and creates its GitHub release from the tested files in $RUNNER_TEMP/sdk, run by sdk-publish.yml.
set -euo pipefail

dir="$RUNNER_TEMP/sdk"
(cd "$dir" && sha256sum -c SHA256SUMS)

# This job can write, so the list holds the drafts the plan cannot see.
drafts=$(gh api --paginate "repos/$GITHUB_REPOSITORY/releases" --jq ".[] | select(.tag_name == \"$TAG\") | .draft")
case "$drafts" in
  *true*)
    echo "::error::a draft release holds $TAG; delete it, then re-run"
    exit 1
    ;;
  *false*)
    for file in "$dir"/*; do
      name=$(basename "$file")
      if [ "$name" = SHA256SUMS ]; then
        continue
      fi
      want="sha256:$(sha256sum "$file" | cut -d' ' -f1)"
      got=$(gh api "repos/$GITHUB_REPOSITORY/releases/tags/$TAG" --jq ".assets[] | select(.name == \"$name\") | .digest")
      [ "$got" = "$want" ] || { echo "::error::the release $TAG holds $name as ${got:-nothing}, not $want"; exit 1; }
    done
    echo "the release $TAG holds these files, so it stays as it is"
    exit 0
    ;;
esac

if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
  tagged=$(git rev-parse "refs/tags/$TAG^{commit}")
  [ "$tagged" = "$(git rev-parse "$REF^{commit}")" ] || { echo "::error::$TAG names $tagged, not $REF"; exit 1; }
fi

# The notes are the version's section of the changelog, below its top heading.
git show "$REF:sdks/$SDK/CHANGELOG.md" | awk '/^## /{n++; next} n == 1' >"$RUNNER_TEMP/notes.md"

# Never the latest release: docs/mac.md downloads shard itself from releases/latest.
flags=(--latest=false)
if [ "$PRERELEASE" = true ]; then
  flags+=(--prerelease)
fi
gh release create "$TAG" --repo "$GITHUB_REPOSITORY" --target "$REF" --title "$TITLE" --notes-file "$RUNNER_TEMP/notes.md" "${flags[@]}" "$dir"/*

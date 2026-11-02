#!/usr/bin/env bash
# Prints the version that the pull requests merged since the last release add up to, or nothing
# when none of them asks for one. Each merged pull request carries one release label, and the
# largest wins (major, then minor, then patch; none releases nothing), so a release run that was
# skipped is covered by the next one. A commit that a release already contains prints nothing.
#
# Usage: next-version.sh <commit>, in a clone with every tag, with gh able to read GITHUB_REPOSITORY.
set -euo pipefail

head=$1
release='^v[0-9]+\.[0-9]+\.[0-9]+$'

containing=$(git tag --contains "$head" --list 'v*')
if grep -qE "$release" <<<"$containing"; then
	exit 0
fi
merged=$(git tag --merged "$head" --list 'v*' --sort=-v:refname)
last=$(grep -m1 -E "$release" <<<"$merged" || true)

rank() {
	case $1 in
	major) echo 3 ;;
	minor) echo 2 ;;
	patch) echo 1 ;;
	*) echo 0 ;;
	esac
}

bump=none
for commit in $(git rev-list "${last:+$last..}$head"); do
	label=$(gh api "repos/$GITHUB_REPOSITORY/commits/$commit/pulls" --jq '
		[.[] | select(.merged_at != null and .base.ref == "main") | .labels[].name | select(startswith("release: "))][0]
		// "" | ltrimstr("release: ")')
	if [ "$(rank "$label")" -gt "$(rank "$bump")" ]; then
		bump=$label
	fi
done

IFS=. read -r major minor patch <<<"${last:-v0.0.0}"
major=${major#v}
case $bump in
major) echo "v$((major + 1)).0.0" ;;
minor) echo "v$major.$((minor + 1)).0" ;;
patch) echo "v$major.$minor.$((patch + 1))" ;;
esac

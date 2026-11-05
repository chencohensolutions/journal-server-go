#!/bin/sh
# Raises the version in version/VERSION and stages it for the next commit (docs/install.md "Versions").
#
# usage: scripts/bump-version.sh [patch|minor|major|<x.y.z>]   (default patch)
#
# The pre-commit hook runs "patch" by itself on commits that change Go code; run "minor" for new features and
# "major" for changes that need every device or install to change too, before committing. A version staged this
# way is kept: the hook only bumps when VERSION is unchanged since the last commit.
set -eu
cd "$(dirname "$0")/.."
file=version/VERSION
current=$(tr -d ' \r\n' < "$file")
major=${current%%.*}
rest=${current#*.}
minor=${rest%%.*}
patch=${rest#*.}
case "${1:-patch}" in
    patch) patch=$((patch + 1)) ;;
    minor) minor=$((minor + 1)); patch=0 ;;
    major) major=$((major + 1)); minor=0; patch=0 ;;
    [0-9]*.[0-9]*.[0-9]*) major=${1%%.*}; rest=${1#*.}; minor=${rest%%.*}; patch=${rest#*.} ;;
    *) sed -n '4p' "$0" | sed 's/^# //' >&2; exit 2 ;;
esac
next="$major.$minor.$patch"
printf '%s\n' "$next" > "$file"
git add "$file"
echo "version $current -> $next"

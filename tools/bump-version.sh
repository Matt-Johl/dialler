#!/bin/sh
# Set the app's version for a merge to main (appstore.md, "Versions").
#
#   tools/bump-version.sh patch|minor|major   next version, build + 1
#   tools/bump-version.sh set 1.2.0           that version, build + 1
#   tools/bump-version.sh current             no change; print and check
#
# MARKETING_VERSION (the App Store version, X.Y.Z) and
# CURRENT_PROJECT_VERSION (the build number, which App Store Connect needs
# to rise with every upload) are written to every target and
# configuration — the app and the PushProvider extension must match, or
# App Store Connect refuses the upload. Prints "X.Y.Z N" on success.
#
# Used inside the merge commit: git merge --no-ff --no-commit <branch>,
# this script, git add, git commit, then git tag -a vX.Y.Z on that commit.
set -eu

cd "$(dirname "$0")/.."
P=ios/Dialler/Dialler.xcodeproj/project.pbxproj

versions=$(sed -n 's/.*MARKETING_VERSION = \(.*\);/\1/p' "$P" | sort -u)
builds=$(sed -n 's/.*CURRENT_PROJECT_VERSION = \(.*\);/\1/p' "$P" | sort -u)
[ "$(echo "$versions" | wc -l | tr -d ' ')" = 1 ] || { echo "targets disagree on MARKETING_VERSION: $versions" >&2; exit 1; }
[ "$(echo "$builds" | wc -l | tr -d ' ')" = 1 ] || { echo "targets disagree on CURRENT_PROJECT_VERSION: $builds" >&2; exit 1; }

# 1.0 reads as 1.0.0.
IFS=. read -r major minor patch <<EOF
$versions
EOF
minor=${minor:-0}
patch=${patch:-0}
build=$builds

case "${1:-}" in
  patch) patch=$((patch + 1)); build=$((build + 1)) ;;
  minor) minor=$((minor + 1)); patch=0; build=$((build + 1)) ;;
  major) major=$((major + 1)); minor=0; patch=0; build=$((build + 1)) ;;
  set)
    echo "${2:-}" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || { echo "usage: $0 set X.Y.Z" >&2; exit 2; }
    IFS=. read -r major minor patch <<EOF
$2
EOF
    build=$((build + 1)) ;;
  current) ;;
  *) echo "usage: $0 patch|minor|major|set X.Y.Z|current" >&2; exit 2 ;;
esac

version="$major.$minor.$patch"
sed -i '' -e "s/MARKETING_VERSION = [^;]*;/MARKETING_VERSION = $version;/" \
          -e "s/CURRENT_PROJECT_VERSION = [^;]*;/CURRENT_PROJECT_VERSION = $build;/" "$P"
echo "$version $build"

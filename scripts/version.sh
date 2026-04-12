#!/usr/bin/env sh
# Usage: [VERSION=vN] version.sh FILE [FILE...]
#
# Replaces version placeholders in each FILE:
#   <span id="v">...</span>  → <span id="v">VERSION</span>
#   ?v=__V__                 → ?v=VERSION
#
# VERSION defaults to `git describe --abbrev=7 --always --tags`, matching the
# format v2-1-gcafejk (last tag, n commits since, g+HEAD short sha).
# Override with the VERSION env var if the script can't see a git repo.

version="${VERSION:-$(git describe --abbrev=7 --always --tags 2> /dev/null)}"
if [ -z "$version" ]; then
  echo "version.sh: cannot derive version (not a git repo; set VERSION=...)" >&2
  exit 1
fi

if [ "$#" -eq 0 ]; then
  echo "$version"
  exit 0
fi

for f in "$@"; do
  sed -i -E \
    -e "s|(<span id=\"v\">)[^<]*(</span>)|\1${version}\2|g" \
    -e "s|\?v=__V__|?v=${version}|g" \
    "$f"
  echo "$f $version"
done

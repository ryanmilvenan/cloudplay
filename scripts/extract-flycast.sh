#!/usr/bin/env bash
# extract-flycast.sh — fetch a flycast AppImage, extract its bundled
# squashfs, and stage the result under vendor/flycast/ for Dockerfile.run
# to COPY in.
#
# Mirrors extract-xemu.sh — see that file for the "why". podman build's
# runtime sandbox blocks AppImage self-extraction on squashfs mount
# syscalls, so we extract outside the build in a one-shot `podman run
# --privileged` container and ship the expanded tree via COPY.
#
# Usage:
#   scripts/extract-flycast.sh                   # v2.6 default
#   FLYCAST_VERSION=v2.7 scripts/extract-flycast.sh
#
# Output: vendor/flycast/{AppRun,usr/bin/flycast,...}

set -euo pipefail

FLYCAST_VERSION="${FLYCAST_VERSION:-v2.6}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

echo "[extract-flycast] version=$FLYCAST_VERSION"

extractor_image="docker.io/library/ubuntu:24.04"
podman image exists "$extractor_image" || {
    echo "[extract-flycast] pulling $extractor_image"
    podman pull "$extractor_image"
}

work="$(mktemp -d -t extract-flycast.XXXXXX)"
trap 'rm -rf "$work"' EXIT

echo "[extract-flycast] workdir=$work"
podman run --rm --privileged -v "$work:/work" "$extractor_image" bash -c "
    set -e
    apt-get -qq update
    apt-get -qq install -y --no-install-recommends wget ca-certificates >/dev/null
    cd /work
    wget -q 'https://github.com/flyinghead/flycast/releases/download/${FLYCAST_VERSION}/flycast-x86_64.AppImage' -O flycast.AppImage
    chmod +x flycast.AppImage
    ./flycast.AppImage --appimage-extract >/dev/null
    rm -f flycast.AppImage
    chmod -R a+rX squashfs-root
"

rm -rf vendor/flycast
mkdir -p vendor
mv "$work/squashfs-root" vendor/flycast

echo "[extract-flycast] staged → vendor/flycast/"
ls -la vendor/flycast/AppRun 2>/dev/null || true
du -sh vendor/flycast

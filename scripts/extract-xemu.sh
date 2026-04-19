#!/usr/bin/env bash
# extract-xemu.sh — fetch an xemu AppImage, extract its bundled squashfs,
# and stage the result under vendor/xemu/ for Dockerfile.run to COPY in.
#
# Why this exists: podman build's runtime sandbox blocks the AppImage
# self-extractor with "exit status 5" even under seccomp=unconfined
# (some interaction with overlayfs + the squashfs mount syscalls that
# we didn't fully chase down). Extracting outside the build — e.g. in
# a one-shot `podman run --privileged` — sidesteps the restriction and
# lets us ship the binaries via a normal COPY.
#
# Usage:
#   scripts/extract-xemu.sh                  # v0.8.5 default
#   XEMU_VERSION=v0.9.0 scripts/extract-xemu.sh
#
# Output: vendor/xemu/{AppRun,usr/bin/xemu,...}

set -euo pipefail

XEMU_VERSION="${XEMU_VERSION:-v0.8.5}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

echo "[extract-xemu] version=$XEMU_VERSION"

# Prefer local ffmpeg/ubuntu if available; otherwise pull a small base.
extractor_image="docker.io/library/ubuntu:24.04"
podman image exists "$extractor_image" || {
    echo "[extract-xemu] pulling $extractor_image"
    podman pull "$extractor_image"
}

work="$(mktemp -d -t extract-xemu.XXXXXX)"
trap 'rm -rf "$work"' EXIT

echo "[extract-xemu] workdir=$work"
podman run --rm --privileged -v "$work:/work" "$extractor_image" bash -c "
    set -e
    apt-get -qq update
    apt-get -qq install -y --no-install-recommends wget ca-certificates >/dev/null
    cd /work
    wget -q 'https://github.com/xemu-project/xemu/releases/download/${XEMU_VERSION}/xemu-${XEMU_VERSION}-x86_64.AppImage' -O xemu.AppImage
    chmod +x xemu.AppImage
    ./xemu.AppImage --appimage-extract >/dev/null
    rm -f xemu.AppImage
    chmod -R a+rX squashfs-root
"

rm -rf vendor/xemu
mkdir -p vendor
mv "$work/squashfs-root" vendor/xemu

echo "[extract-xemu] staged → vendor/xemu/"
ls -la vendor/xemu/AppRun vendor/xemu/usr/bin/xemu 2>/dev/null || true
du -sh vendor/xemu

#!/usr/bin/env bash
# moon-tail.sh — live tail of moon logs filtered for iteration signal.
#
# Default: filter for XEMU-* prefixes + panic + error in the dev container.
# Pass a target ("dev" or "prod") and/or a grep regex to override.
#
# Examples:
#   scripts/moon-tail.sh
#   scripts/moon-tail.sh dev '\[XEMU-VIDEO\]|frame'
#   scripts/moon-tail.sh prod 'Handshake|identity='

set -euo pipefail

MOON="${MOON_HOST:-rocks@moon.local}"

target="${1:-dev}"
pattern="${2:-\\[XEMU-|panic|error|\\[INPUT-DIAG\\]}"

case "$target" in
    dev)  container="cloudplay-dev" ;;
    prod) container="cloudplay" ;;
    *)    container="$target" ;;
esac

printf '\033[1;34m[moon-tail]\033[0m container=%s filter=%q\n' "$container" "$pattern"

exec ssh "$MOON" "podman logs -f --tail 100 $container 2>&1 | grep --line-buffered -iE '$pattern'"

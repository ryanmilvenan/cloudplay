#!/usr/bin/env bash
# dev-sync.sh — Mac→moon edit/build loop for cloudplay's native-emulator work.
#
# Steps:
#   1. rsync the local working tree to moon:~/containers/cloudplay-dev/src/
#   2. podman exec cloudplay-dev go build -o /out/worker ./cmd/worker
#   3. optionally: run a harness or restart the dev worker
#
# Goal: ≤25 s steady-state round-trip on empty-diff reruns (cached Go builds).
# This is *separate* from /deploy-cloudplay; it never touches the production
# container or the prod image.
#
# Usage:
#   scripts/dev-sync.sh                 # sync + build worker
#   scripts/dev-sync.sh test ./pkg/...  # sync + go test
#   scripts/dev-sync.sh exec <cmd>      # sync + arbitrary command inside container
#   scripts/dev-sync.sh harness xemu-canary  # sync + build+run one of the tools/ harnesses
#   scripts/dev-sync.sh sync-only       # just rsync, no build

set -euo pipefail

MOON="${MOON_HOST:-rocks@moon.local}"
REMOTE_SRC="/home/rocks/containers/cloudplay-dev/src"
CONTAINER="cloudplay-dev"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

say() { printf '\033[1;34m[dev-sync]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[dev-sync]\033[0m %s\n' "$*" >&2; exit 1; }

sync_tree() {
    say "rsync → $MOON:$REMOTE_SRC/"
    rsync -a --delete \
        --exclude '.git' \
        --exclude 'bin' \
        --exclude 'node_modules' \
        --exclude '.claude' \
        --exclude 'assets/games' \
        --exclude 'assets/cores' \
        ./ "$MOON:$REMOTE_SRC/"
}

remote_exec() {
    ssh "$MOON" "podman exec $CONTAINER $*"
}

# rcheevos is a vendored C library that must be built before the Go linker
# can resolve -lrcheevos. The prod Dockerfile does this as a dedicated RUN
# step, but /src is a bind-mount in the dev container so that artifact doesn't
# persist. Build it once per mount; the .a file then lives on the bind mount
# and subsequent dev-sync calls no-op via `test -f`.
ensure_rcheevos() {
    say "ensure librcheevos.a (one-time per bind-mount)"
    remote_exec sh -c "'
        set -e
        LIB=/src/pkg/worker/rcheevos/upstream/build/librcheevos.a
        if [ -f \"\$LIB\" ]; then exit 0; fi
        cd /src/pkg/worker/rcheevos/upstream
        mkdir -p build && cd build
        gcc -c -O2 -fPIC -I../include -I../src \$(find ../src -name \"*.c\" -not -name \"rc_libretro.c\")
        ar rcs librcheevos.a *.o
        rm -f *.o
    '"
}

# Use the same build recipe the production Dockerfile uses, so behavior
# matches what /deploy-cloudplay produces byte-for-byte.
build_worker() {
    ensure_rcheevos
    # GO_TAGS must be a make arg (not env) — Makefile has explicit
    # `GO_TAGS=` at top which env can't override.
    say "make GO_TAGS=static,st,vulkan,nvenc build.worker (inside $CONTAINER)"
    remote_exec sh -c "'cd /src && make GO_TAGS=static,st,vulkan,nvenc build.worker'"
}

build_coordinator() {
    say "make build.coordinator (inside $CONTAINER)"
    remote_exec sh -c "'cd /src && make build.coordinator'"
}

run_tests() {
    ensure_rcheevos
    say "go test -tags static,st,vulkan,nvenc $* (inside $CONTAINER)"
    # Build tags must match the production build; several packages
    # reference tag-gated NVENC/Vulkan types that won't compile otherwise.
    # shellcheck disable=SC2086
    remote_exec sh -c "'cd /src && go test -tags static,st,vulkan,nvenc $*'"
}

run_harness() {
    local harness="$1"
    shift || true
    local harness_dir="tools/$harness"
    [[ -d "$harness_dir" ]] || fail "unknown harness: $harness (expected $harness_dir/ in repo)"
    say "build+run harness: $harness"
    # shellcheck disable=SC2029
    remote_exec sh -c "'cd /src && go build -o /out/$harness ./$harness_dir && /out/$harness $*'"
}

cmd="${1:-build}"; [[ $# -gt 0 ]] && shift || true

case "$cmd" in
    sync-only)
        sync_tree
        ;;
    build|worker)
        sync_tree
        build_worker
        ;;
    coordinator)
        sync_tree
        build_coordinator
        ;;
    test)
        sync_tree
        run_tests "${@:-./...}"
        ;;
    harness)
        [[ $# -ge 1 ]] || fail "usage: $0 harness <name> [args...]"
        sync_tree
        run_harness "$@"
        ;;
    exec)
        [[ $# -ge 1 ]] || fail "usage: $0 exec <cmd> [args...]"
        sync_tree
        remote_exec "$@"
        ;;
    *)
        fail "unknown subcommand: $cmd (expected: sync-only|build|test|harness|exec|coordinator)"
        ;;
esac

say "done"

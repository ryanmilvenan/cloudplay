# Test hygiene TODO

Tracker for tests we've **temporarily skipped or deleted** during the xemu
native-emulator work so iteration signal stays clean. Come back to this list
once Phase 7 lands — resolve every entry before declaring the feature done.

Search marker: `XEMU-WIP` (every skip calls `t.Skip("XEMU-WIP: ...")` so
`grep -rn XEMU-WIP pkg/` finds the full set).

## Skipped tests

| Test | File | Why skipped | Fix strategy |
|---|---|---|---|
| `TestLibraryScan` | `pkg/games/library_test.go:13` | Requires three sample ROMs at `../../assets/games/{nes,gba}/...` which are gitignored and don't exist in the dev bind-mount. | Option A: auto-skip when fixture dir is missing. Option B: commit tiny homebrew fixtures specifically for this test (under `pkg/games/testdata/` not `assets/games/`). |
| `TestFrontendLoadCore` and any other `frontend_test.go` tests that hit real ROMs | `pkg/worker/caged/libretro/frontend_test.go` | Same `assets/games/` dependency — references e.g. `assets/games/gba/Sushi The Cat.gba`. | Same as above. Move fixtures into `pkg/worker/caged/libretro/testdata/` (committed, tiny). |
| `TestRoom*` | `pkg/worker/room/room_test.go`, `router_test.go` | Needs a live video device (`XDG_RUNTIME_DIR` + Xvfb/DRI). Fails in CI-like envs with "No available video device". | Either gate on env-var presence and `t.Skip` cleanly (preferred) or run these against the dev container's Xvfb display. |
| `TestWebsocket` | `pkg/com/net_test.go:40` | Flaky concurrency/timing test — fails intermittently under load. Net is not our scope right now. | Investigate the RPC wait group / timing; replace sleeps with synchronized completion. |

## Deleted tests

| Test | File | Why deleted | Notes |
|---|---|---|---|
| `TestNormalizeCodec` | `pkg/network/webrtc/webrtc_test.go` | Referenced `normalizeCodec()` which was inlined into `newTrack()` at some point; the test couldn't even build. | If we want to re-expose `normalizeCodec` as a helper (and teach it to trim whitespace, which was the old behavior the test asserted), revive it in a post-Phase-7 cleanup PR. `TestNewTrackMapsH264NVENCToH264Mime` in the same file covers the core lowercase contract already. |

## Infrastructure adjustments (not bugs, documented for reference)

- `scripts/dev-sync.sh` test runner passes `CGO_LDFLAGS="-lm"` so `pkg/encoder/h264` links libx264 under `go test`. The prod Makefile provides this via its own LDFLAGS chain. No revisit needed unless we drop x264.
- Tests are run with `-tags static,st,vulkan,nvenc` — matches prod.

## Revisit protocol

When addressing this file, work top-to-bottom. Each row's fix should be its
own commit. At the end, delete this file (or update it if new entries accumulate).

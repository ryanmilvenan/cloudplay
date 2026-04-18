# xemu-canary

Deterministic validator for the xemu **LD_PRELOAD GL-capture shim** used by the
native-emulator backend (Phase 3).

## What it does

Reuses the exact capture shim that the xemu backend uses in production, but
drives it against a known GL demo instead of xemu. That lets us assert the
capture path byte-for-byte against golden frames — no hand-waving, no "looks
right on screen."

1. Build `pkg/worker/caged/xemu/videocap_preload.c` → `videocap_preload.so`.
2. Spawn a tiny OpenGL demo rendering one of three fixed RGBA patterns
   (solid-color grid, checkerboard with frame-counter overlay, rainbow gradient).
3. `LD_PRELOAD=...` the shim into the demo. Capture 10 frames per pattern.
4. SHA256 each captured frame; diff against `testdata/frame_<pattern>_<n>.sha256`.
5. Exit 0 iff every frame matches.

## Why standalone

Running the shim against xemu first requires xemu + Xvfb + BIOS files + real
ROMs. This harness needs none of that. It's the first thing to go green in
Phase 3, so we can iterate on the shim itself without the rest of the xemu
machinery confusing the signal.

## Running (on moon)

```bash
scripts/dev-sync.sh harness xemu-canary
```

Or from inside the dev container:

```bash
go build -o /out/xemu-canary ./tools/xemu-canary && /out/xemu-canary
```

## Golden regeneration

When the shim legitimately changes its output format, regenerate goldens:

```bash
/out/xemu-canary --write-goldens
```

Review the resulting `testdata/frame_*.sha256` diff before committing.

## Status

**Phase 0**: scaffold only (`main()` exits with "not implemented").
**Phase 3**: full implementation lands together with the shim.

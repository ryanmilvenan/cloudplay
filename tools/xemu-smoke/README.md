# xemu-smoke

Process-lifecycle smoke test for the xemu native backend (Phase 2+).

## What it does

Exercises the Xvfb-display + xemu-process primitives that the backend uses,
without any browser, WebRTC, or capture path in the loop.

1. Allocate an Xvfb display via `pkg/worker/caged/xemu/xvfb.go`.
2. Start xemu via `pkg/worker/caged/xemu/process.go` (no ROM — BIOS only).
3. Sleep 5 s to let it settle.
4. SIGTERM. Assert:
   - Process exits within 2 s.
   - No lingering `xemu` or `Xvfb` in `pgrep` after exit.
   - No zombies.
5. Repeat 10 times. Exit 0 iff every iteration is clean.

**Phase 3 extension:** after boot, invoke the GL-capture shim for one frame
and SHA256 it against `testdata/dashboard_boot_frame_0.sha256`. That catches
regressions in either the process path or the capture path.

## BIOS requirement

xemu needs Xbox MCPX + flash BIOS files to boot. Supply them on moon at
`/home/rocks/containers/cloudplay-dev/xemu-bios/`; the dev-container quadlet
bind-mounts that to `/xemu-bios/` inside the container.

## Running (on moon)

```bash
scripts/dev-sync.sh harness xemu-smoke
```

## Status

**Phase 0**: scaffold only.
**Phase 2**: lifecycle assertions (steps 1–5).
**Phase 3**: boot-frame SHA comparison added.

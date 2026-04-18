# Backend architecture

> Update this diagram when you change how the backend is structured.
> See [../CLAUDE.md](../CLAUDE.md) for what counts as "structural".

Two Go binaries in one repo: **coordinator** (the thin control plane) and **worker** (per-session emulator host). Both ship as one podman image; the container's entrypoint starts coordinator and supervises worker restarts.

Runtime deployment: moon bind-mounts `web/` and config.yaml; the container owns everything else in the image. The podman quadlet unit file lives at `systemd/cloudplay.container` in this repo and is the source of truth for `~/.config/containers/systemd/cloudplay.container` on moon.

```mermaid
flowchart TB
    subgraph ingress["Ingress on queen"]
        traefik["Traefik<br/>chain-oauth2 (games.*)<br/>chain-claude-test (games-dev.*)"]
    end

    subgraph container["podman quadlet: cloudplay (on moon)"]
        subgraph coord["coordinator (cmd/coordinator)"]
            coordhub["pkg/coordinator<br/>hub · userhandlers · workerhandlers<br/>HTTP + WS"]
            coordfs["httpx.FileServer → /usr/local/share/cloud-game/web<br/>(bind-mounted)"]
        end
        subgraph worker["worker (cmd/worker)"]
            room["pkg/worker/room<br/>GameSession · broadcastRoomMembers"]
            coordh["pkg/worker/coordinatorhandlers.go"]
            media["pkg/worker/media<br/>pipe: core frame → encoder → WebRTC"]
            caged["pkg/worker/caged<br/>backend registry (Manager)<br/>libretro · xemu"]
            recorder["pkg/worker/recorder"]
            cloud["pkg/worker/cloud (saves)"]
        end
        subgraph libretro["pkg/worker/caged/libretro"]
            nanoarch["nanoarch<br/>cgo bridge to libretro API"]
            graphics["graphics/<br/>gl + vulkan HW render"]
            manager["manager<br/>core discovery"]
            thread["thread<br/>main-thread pinning"]
        end
        subgraph xemu["pkg/worker/caged/xemu"]
            xcaged["caged.go<br/>app.App impl (Phase 1: stub gradient)"]
            xproc["process.go · xvfb.go<br/>(Phase 2)"]
            xvideo["videocap_preload.c · videocap.go<br/>LD_PRELOAD GL capture (Phase 3)"]
            xaudio["audiocap.go<br/>pw-record (Phase 4)"]
            xinput["input.go<br/>uinput virtual gamepad (Phase 5)"]
        end
        subgraph encpkg["pkg/encoder"]
            yuv["yuv<br/>RGBA→I420"]
            nvenc["nvenc<br/>CUDA H.264"]
            h264["h264 (libx264 sw)"]
            vpx["vpx"]
            opus["opus (audio)"]
            color["color/<br/>bgra · rgb565 · rgba"]
        end
        subgraph netpkg["pkg/network"]
            httpx["httpx"]
            websocket["websocket"]
            webrtc["webrtc<br/>Pion + single-port ICE"]
            socket["socket"]
        end

        common["pkg/api<br/>endpoint codes · encode/decode<br/>identity from headers"]
        config["pkg/config<br/>config.yaml + env"]
        games["pkg/games<br/>ROM index"]
        logger["pkg/logger"]
        resampler["pkg/resampler<br/>audio"]
        monitoring["pkg/monitoring"]

        coordhub -- WS / HTTP --> client["Client (web)"]
        coordhub -- gRPC-like over WS --> coordh
        coordh --> room
        room --> caged
        caged --> libretro
        caged --> xemu
        nanoarch <-.cgo.-> corelib["libretro core<br/>(lrps2, mupen64plus, …)"]
        nanoarch --> graphics
        graphics --> media
        xcaged -.spawns.-> xproc
        xproc <-.ld_preload.-> xemuproc["xemu process<br/>(external, Xvfb-backed)"]
        xvideo --> media
        xaudio --> media
        xinput --> xproc
        media --> yuv
        media --> nvenc
        media --> h264
        media --> vpx
        media --> opus
        media --> webrtc
        webrtc -- RTP / DC --> client

        coord -- api --> common
        worker -- api --> common
    end

    traefik --> coord
    client --> traefik

    subgraph host["moon filesystem (bind-mounted into container)"]
        webfs["~/containers/cloudplay/web/<br/>(rsync deploys)"]
        cfgfs["~/containers/cloudplay/config/config.yaml"]
        romsfs["~/containers/cloudplay/games/"]
        coresfs["~/containers/cloudplay/cores/"]
        savesfs["~/containers/cloudplay/saves/"]
    end

    webfs -- ro bind --> coordfs
    cfgfs -- ro bind --> config
    romsfs -- ro bind --> games
    coresfs -- ro bind --> manager
    savesfs -- rw bind --> cloud

    classDef ext fill:#e2e3e5,stroke:#383d41;
    class client,corelib,xemuproc,traefik,ingress ext;
```

## Notable invariants

- **Coordinator is the only thing clients talk to.** Workers never expose a public port; all traffic from a worker out goes through WebRTC (media/DC) or the coordinator WS fanout.
- **Auth trust boundary is Traefik**, not the app. `pkg/api/identity.go` reads `X-Auth-Request-*` headers set by oauth2-proxy (or the bypass middleware on `games-dev`). Never trust these headers when the coordinator is reachable directly.
- **GameSession identity**: each WS connection carries a pocket-id identity; on state-machine events (join/change-slot/leave) the worker re-broadcasts the full roster via `api.PT 207 (RoomMembers)` for every client.
- **Zero-copy video path**: Vulkan core → extmem → CUDA → NVENC, bypassing host CPU when the core renders via Vulkan. GL cores fall back to `readFramebuffer → yuv420 → encoder`.
- **One container, two processes**: coordinator and worker share the image. Dockerfile.run's CMD supervises worker restarts; a hard crash keeps coordinator alive and the supervisor forks a fresh worker.
- **Bind-mounted paths** let a `web/` rsync deploy in seconds, a config.yaml edit + `systemctl restart` avoid a rebuild, and ROM/core/save directories be managed independently of the image.
- **GPU access uses CDI** (`AddDevice=nvidia.com/gpu=all`), not hand-curated driver-versioned bind mounts, so the quadlet survives NVIDIA driver upgrades without edits.
- **Second backend via native process**: `pkg/worker/caged/xemu` runs xemu as an external OS process alongside libretro. `caged.Manager` dispatches on `ModName` (`libretro` / `xemu`); `app.App` is the shared contract so `room/` and `media/` stay backend-agnostic. As of Phase 1 the xemu backend is a frame-generating stub; Phase 2+ adds real process / capture / input primitives. xemu stays off by default (`xemu.enabled: false`).

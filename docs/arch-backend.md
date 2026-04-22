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
            room["pkg/worker/room<br/>GameSession · broadcastRoomMembers<br/>cast.go: WithEmulator / IsLibretro"]
            coordh["pkg/worker/coordinatorhandlers.go<br/>HandleGameStart → dispatch by game.Backend"]
            media["pkg/worker/media<br/>pipe: core frame → encoder → WebRTC"]
            caged["pkg/worker/caged<br/>backend registry (Manager)<br/>Load(Libretro) + Load(Xemu) + Load(Flycast) at startup"]
            recorder["pkg/worker/recorder"]
            cloud["pkg/worker/cloud (saves)"]
        end
        subgraph libretro["pkg/worker/caged/libretro"]
            nanoarch["nanoarch<br/>cgo bridge to libretro API"]
            graphics["graphics/<br/>gl + vulkan HW render"]
            manager["manager<br/>core discovery"]
            thread["thread<br/>main-thread pinning"]
        end
        subgraph nativeemu["pkg/worker/caged/nativeemu (shared native-emu scaffolding)"]
            neproc["process.go<br/>generic Cmd supervisor"]
            nexvfb["xvfb.go<br/>virtual X display"]
            nevideo["videocap.go<br/>ffmpeg x11grab → app.Video"]
            neaudio["audiocap.go · pipewire.go<br/>per-session pipewire + parec → app.Audio"]
            nepad["virtualpad.go<br/>pure-Go uinput Xbox-360 pad<br/>RetroPad wire → EV_KEY/EV_ABS"]
        end
        subgraph xemu["pkg/worker/caged/xemu"]
            xcaged["caged.go<br/>app.App impl · composes nativeemu<br/>stub emitter → real frames on first live frame"]
            xconf["xemuconfig.go<br/>BIOS discovery + xemu.toml template + env"]
        end
        subgraph flycast["pkg/worker/caged/flycast"]
            fcaged["caged.go<br/>app.App impl · composes nativeemu<br/>Dreamcast 4-port fanout · stub emitter"]
            fconf["flycastconfig.go<br/>emu.cfg template + env"]
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
        caged --> flycast
        xemu --> nativeemu
        flycast --> nativeemu
        nanoarch <-.cgo.-> corelib["libretro core<br/>(lrps2, mupen64plus, …)"]
        nanoarch --> graphics
        graphics --> media
        neproc <-.x11grab / pulse / uinput.-> emuproc["xemu or flycast process<br/>(external, Xvfb-backed)"]
        nevideo --> media
        neaudio --> media
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
    class client,corelib,emuproc,traefik,ingress ext;
```

## Notable invariants

- **Coordinator is the only thing clients talk to.** Workers never expose a public port; all traffic from a worker out goes through WebRTC (media/DC) or the coordinator WS fanout.
- **Auth trust boundary is Traefik**, not the app. `pkg/api/identity.go` reads `X-Auth-Request-*` headers set by oauth2-proxy (or the bypass middleware on `games-dev`). Never trust these headers when the coordinator is reachable directly.
- **GameSession identity**: each WS connection carries a pocket-id identity; on state-machine events (join/change-slot/leave) the worker re-broadcasts the full roster via `api.PT 207 (RoomMembers)` for every client.
- **Zero-copy video path**: Vulkan core → extmem → CUDA → NVENC, bypassing host CPU when the core renders via Vulkan. GL cores fall back to `readFramebuffer → yuv420 → encoder`.
- **One container, two processes**: coordinator and worker share the image. Dockerfile.run's CMD supervises worker restarts; a hard crash keeps coordinator alive and the supervisor forks a fresh worker.
- **Bind-mounted paths** let a `web/` rsync deploy in seconds, a config.yaml edit + `systemctl restart` avoid a rebuild, and ROM/core/save directories be managed independently of the image.
- **GPU access uses CDI** (`AddDevice=nvidia.com/gpu=all`), not hand-curated driver-versioned bind mounts, so the quadlet survives NVIDIA driver upgrades without edits.
- **Native-process backends unified via `pkg/worker/caged/nativeemu`**: `xemu` (original Xbox) and `flycast` (Sega Dreamcast) each run their emulator as an external OS process alongside libretro. Both compose the shared `nativeemu.{Process, Xvfb, Videocap, PipeWireSession, Audiocap, VirtualPad}` primitives rather than duplicating plumbing. `caged.Manager` dispatches on `ModName` (`libretro` / `xemu` / `flycast`); `app.App` is the shared contract so `room/` and `media/` stay backend-agnostic. Video capture: `ffmpeg -f x11grab` pipe reading the emu's Xvfb display — the original LD_PRELOAD GL hook captured xemu's offscreen ImGui context rather than the game output, see `docs/capture-path-not-taken-ld-preload.md`. Audio: each cage spawns its own pipewire+wireplumber+pipewire-pulse triplet under a private `XDG_RUNTIME_DIR`, the emu connects via SDL pulse, and parec feeds 48 kHz S16LE chunks through app.Audio at 100 Hz (10 ms chunks). Input: pure-Go uinput device emulating Microsoft Xbox 360 vid/pid (045e:028e) with xpad-style button codes — SDL's built-in gamecontrollerdb mapping applies automatically in both emulators. `HandleGameStart` routes via an effective-backend switch: per-launch `?backend=<modname>` query param wins, then the library-scan `GameMetadata.Backend`, else the default libretro path. Save/Load/Reset handlers gate on `room.IsLibretro(r.App())` and return `ErrPacket` for non-libretro backends. Stub frame emitter is parked on the first live frame so the room only ever sees one stream. Native backends stay off by default (`xemu.enabled: false`, `flycast.enabled: false`).
- **Backend override precedence**: `CLOUDPLAY_BACKEND_<SYSTEM>` env var (deploy-wide) → config.yaml core `backend:` field (per-system default captured at library scan) → per-launch `?backend=<modname>` query param from the browser (wins at dispatch time). Empty at every layer falls through to libretro. The env-var and config layers both resolve inside `Emulator.GetBackend(system)`; the per-launch layer is applied in `HandleGameStart` against `rq.Backend`.
- **uinput permissions on host**: `/dev/uinput` must be writable by the container's mapped user. The `systemd/99-cloudplay-uinput.rules` udev rule enforces `GROUP=input, MODE=0660` on both `/dev/uinput` and event nodes that match our virtual-pad names. Rootless podman needs either (a) the `input` gid in the user's `/etc/subgid` + `PodmanArgs=--group-add keep-groups`, or (b) the node chowned to the rootless user. Option (b) is tested/documented; option (a) is the long-term fix.

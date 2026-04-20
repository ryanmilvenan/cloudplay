# Perf profile — 2026-04-20

Initial CDP-driven WebRTC profile of Power Stone 2 (Dreamcast/flycast) and
Halo: CE (Xbox/xemu). Full CSVs + summaries in `/tmp/{ps2,halo,halo2}.csv`.

## Tool

`scripts/cdp_perf_profile.py` polls `RTCPeerConnection.getStats()` via CDP
at 2 Hz for N seconds, writes per-sample raw counters and deltas to CSV,
and a human-readable summary with means / percentiles / autocorrelation.
The matching `webrtc.statsRaw()` export bypasses the `connected`-flag
guard in `webrtc.js` since Chrome's iceConnectionState occasionally
reaches media-flowing state without ever firing the `'connected'`
transition that flips our flag.

## Setup caveats to cross-check

**These measurements came from the CDP Chrome at `127.0.0.1:9222`, not
the user's primary browser.** Two environment quirks pushed the numbers
worse than reality:

1. **Network Info API reports 3G / 1.4 Mbps / 300ms RTT** in the CDP
   Chrome. Real Chrome on a healthy network reports much better.
   WebRTC's internal BWE (bandwidth estimation) is independent of the
   NetInfo API, but this suggests the CDP profile may be running under
   different media-stack defaults. A side-by-side against the user's
   regular browser via `chrome://webrtc-internals/` (dump JSON and
   re-run the same analysis) would tell us whether the raw numbers
   below are artifact or signal.
2. **CDP tab visibility**: Chrome aggressively throttles hidden tabs
   (RAF 1 Hz, decoder pause). The profiler now calls `Page.bringToFront`
   before sampling, which brought `document.visibilityState` to
   `visible` — but backgrounded Chrome windows on macOS may still see
   some throttling. Ideally run the profiler while the CDP Chrome has
   the OS focus.

## Server config (from worker logs, 22:04 UTC)

- `h264_nvenc` encoder, preset=p6, tune=ll, profile=baseline
- Target bitrate **15000 kbps** (15 Mbps) — high for a realtime WebRTC
  video over public Internet
- GOP 60 (1 second between keyframes at 60 fps)
- **Zero-copy path NOT armed** — `backend=opengl-sdl` reports
  `SupportsZeroCopy=false`, so frames round-trip through CPU readback
  (`libyuv`, stride 1904) before NVENC. Likely fine throughput-wise,
  but means every frame copies GPU→system memory→GPU.

## Measured behaviour

Power Stone 2 (dreamcast/flycast), 120 s sample, tab visible:

| metric | value |
|---|---|
| frames received rate | 12.3 fps avg |
| framesPerSecond (instantaneous) | p50=51.5, mean=41.2, min=1, max=128 |
| freeze events | 23 (one every ~5.2 s) |
| total freeze time | 84 s (70 % of session) |
| packets lost | 58.7 % of total |
| p95 inter-frame delay delta | 1.3 s |
| worst inter-frame delay delta | 15.4 s |

Halo: CE (xbox/xemu), 120 s sample, tab foregrounded:

| metric | value |
|---|---|
| frames received rate | 5.8 fps avg |
| framesPerSecond (instantaneous) | p50=59, mean=41.6, min=1, max=76 |
| freeze events | 6 (one every ~20 s) |
| total freeze time | 107 s (90 % of session) |
| packets lost | 60.9 % |

The structural signal both runs share:

- When the stream **is** flowing, `framesPerSecond` instantaneous sits
  at the target (55–59 fps). So the decoder keeps up fine.
- But the stream pauses for **15–50 seconds at a time**, then bursts
  back to life with a few seconds of frames at 60 fps, then pauses again.
- `totalInterFrameDelay` grows linearly with wall-clock time during
  the pause, i.e. **the server is not emitting frames during the pause**.
  That's not packet loss in the transport, that's the encoder (or the
  emulator feeding it) stopping.

## Hypotheses, rank-ordered by suspicion

1. **The CDP tab's media pipeline is throttled somewhere we can't see.**
   The 3G / 1.4 Mbps NetInfo reading is the smoking gun. First action
   should be running the same profiler in the user's primary browser
   (chrome://webrtc-internals/ → "Create Dump") to see whether these
   multi-second gaps show up outside the automation profile at all.
2. **Pacer / congestion control is mis-tuned at 15 Mbps target.** On a
   consumer upstream link, 15 Mbps sustained realtime video is
   ambitious; if BWE estimates the link lower and clamps send rate, the
   encoder would back off and we'd see gaps. Config knob:
   `encoder.video.nvenc.bitrate` in config.yaml. Lowering to 8000 kbps
   would be the first experiment.
3. **Zero-copy isn't armed.** CPU readback at 60 fps × 640×480 RGBA is
   ~70 MB/s — not a bottleneck by itself, but the readback creates a
   GPU↔CPU synchronization point every frame. `opengl-sdl` backend
   would need Vulkan-external-memory to arm zero-copy; flycast's GL
   backend doesn't. Low-priority for Dreamcast but worth verifying
   that the Halo/xemu path is also CPU-readback.
4. **x11grab pacing for xemu**. Halo is captured via `ffmpeg x11grab`
   off Xvfb, which pulls at a fixed framerate and can desync under
   load. Would show as regular pacing glitches rather than long stalls.
5. **Worker Go runtime GC pauses**. A long STW GC could pause frame
   submission for 100s of ms — unlikely to reach multi-second gaps, but
   a side channel to rule out.

## Next steps

1. Re-run profiler from the user's primary browser (non-CDP Chrome) or
   export a dump from `chrome://webrtc-internals/` during a real play
   session — decide whether the stall pattern is real.
2. If real: try `encoder.video.nvenc.bitrate: 8000` in config.yaml and
   re-profile to see whether the gaps shrink.
3. Compare against a **local** flycast running the same ROM on the Mac
   — a few minutes of direct-device play validates whether the core
   itself is pacing badly versus the WebRTC pipeline.

## Files

- `scripts/cdp_perf_profile.py` — the profiler
- `docs/perf-profile-2026-04-20.md` — this doc
- `/tmp/ps2.csv`, `/tmp/halo.csv`, `/tmp/halo2.csv` — raw samples
- `/tmp/*.summary.txt` — per-run stat summaries

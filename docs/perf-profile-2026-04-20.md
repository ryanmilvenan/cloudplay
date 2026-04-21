# Perf profile — 2026-04-20

Two data points, second one is the one to trust:

1. **CDP-driven profile** via `scripts/cdp_perf_profile.py` against the
   games-dev tab in the automation Chrome. Numbers looked catastrophic
   (60% packet loss, 90% frozen). **Artifact** — the automation Chrome
   reports `Network Info: 3G / 1.4 Mbps / 300ms RTT` and has other
   media-stack quirks that don't reflect a real session. Kept around
   because the tool is still useful; don't trust the specific numbers.

2. **`chrome://webrtc-internals/` dump from the user's primary browser**
   playing Power Stone 2 on `games.milvenan.technology`. This is the
   real answer.

## Real-browser measurement

51 seconds of Power Stone 2, Dreamcast/flycast, from the user's Mac via
Safari-adjacent Chrome build (VideoToolbox HW decoder). Summary:

| metric | value |
|---|---|
| **average framerate** | **60.0 fps** ← target hit |
| bandwidth | 11.47 Mbps (of 15 Mbps target) |
| packets received | 66,265 |
| **packet loss** | **0** ← perfect transport |
| NACKs (retransmit requests) | 0 |
| PLIs (keyframe requests) | 0 |
| frames dropped | 9 over 51 s |
| **freeze events** | **9**, one every ~5.7 s |
| **total freeze time** | **1.53 s** (3.0 % of session) |
| decoder | ExternalDecoder (VideoToolboxVideoDecoder) |

Per-second `framesReceived` delta over the session:

```
t+ 1s  51     dip (startup)
t+ 2–5  63   (makeup — decoder consumed stash, Chrome reports ahead of nominal)
t+ 6s  57    dip (-3 frames)
t+ 12s 52    dip
t+ 24s 57    dip
t+ 25s 57    dip (two-sample stretch)
t+ 35s 52    dip
t+ 46s 52    dip
t+ 50s 56    dip
```

Inter-drop intervals: 5, 6, 12, 10, 11, 11 seconds. The **"rhythmic
drop" pattern is real** but it's small (3 %) and regular — a minor
drop every ~11 seconds on average.

## What's causing the rhythm

Derived from the jitter-buffer stats:

- **mean inter-frame delay** = 50.99 s / 3060 frames = **16.66 ms** (60 fps on the dot)
- **stdev of inter-frame delay** ≈ 26 ms (derived from totalSquaredInterFrameDelay)
- **jitterBufferTargetDelay** = 39 ms (Chrome's target buffer)
- **jitterBufferDelay** = 26 ms average (what it's actually holding)

So the mean is dead-on 60 fps, but the inter-frame delay has a 26 ms
standard deviation — meaning occasional frames arrive ≥40 ms late. The
jitter buffer (target 39 ms) is sized just above the normal spread, so
a small fraction of over-budget arrivals punch through and drop.

Audio side supports the story: `insertedSamplesForDeceleration = 120`
(120 samples at 48 kHz = 2.5 ms) — Chrome is slowing audio playback by
a hair to keep it aligned with video. That's a clock-drift sign —
server's 60 fps cadence and the Mac's 60 Hz display clock aren't
bit-for-bit identical, so the audio pipeline periodically nudges.

Consistent explanations for the ~11 s beat:

1. **Encoder-side pacing harmonics**. NVENC + the Go media pipe
   probably emit frames at "60 per wall-second" but not at precisely
   16.66 ms intervals; occasional drift triggers a > 40 ms gap every
   ~11 s (≈ 660 frames of built-up drift between corrections).
2. **GOP-adjacent packet bursts**. Keyframes at GOP 60 = one per second
   — bigger frames → more packets → wider spread through the RTP pacer,
   occasionally enough to over-shoot the jitter budget.
3. **Display/audio clock sync**. Every ~11 s, a full-audio-frame worth
   of drift accumulates and Chrome's A/V sync logic drops one video
   frame to realign. Matches the decelerated-samples counter.

## Recommendations

Triage: the session is **objectively healthy**. 60 fps target, zero
loss, 97 % sustained playback. The 3 % freeze window is noticeable
because it's rhythmic, not because it's big.

If we want to polish the remaining 3 %:

1. **Lower `encoder.video.nvenc.bitrate` to 8000 kbps**. Smaller frames
   → less packet-burst jitter, less chance of over-budget arrivals.
   Cost: some visible compression on complex scenes. Cheap experiment,
   easy rollback.
2. **Match GOP to a longer interval** (e.g. 120 = 2 s between
   keyframes). Reduces the keyframe-burst frequency. Cost: slower
   recovery from packet loss (we have zero loss, so cost is moot).
3. **Bump `jitterBufferTargetDelay`** (receiver-side — WebRTC playout
   delay hint) from ~40 ms to ~80 ms. Would absorb the late-frame tail
   at the cost of 40 ms more input latency.

Zero-copy is off (`SupportsZeroCopy=false` on flycast's OpenGL
backend) — the CPU readback isn't causing this pattern at 60 fps but
arming it would remove the GPU↔CPU sync point. Separate workstream
from this perf issue.

## Files

- `scripts/cdp_perf_profile.py` — live CDP-driven profiler
- `docs/perf-profile-2026-04-20.md` — this doc
- Raw dumps: `/tmp/webrtc_dump.txt` (real browser, authoritative)
- CDP CSVs: `/tmp/ps2.csv`, `/tmp/halo.csv`, `/tmp/halo2.csv` (artifact — keep for method reference, not numbers)

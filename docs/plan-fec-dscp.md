# Plan: FEC + DSCP for wireless-robust WebRTC video

Status: not started. Captured 2026-04-21 so we don't lose the context.

## Why

User reports every-few-seconds micro-freezes over Wi-Fi that vanish
when hardwired. Traced (see `docs/perf-profile-2026-04-20.md`) to
"9 freezes per 51 s, 3 % of session" with otherwise-perfect network.
Root cause is almost certainly **Apple's AWDL peer-discovery
dwells** — the client radio hops off the associated AP channel every
50–100 ms for ~16 ms to scan for AirDrop / Handoff / Sidecar peers.
During the hop, queued RTP packets are delayed; Chrome's jitter
buffer occasionally over-spills and drops a frame.

User is on M2 Mac (Wi-Fi 6, no 6 GHz sidestep available). Pinning
UniFi to channel 149 helps but doesn't eliminate. The right answer
for the server is to make WebRTC **tolerate** bursty wireless loss
regardless of what individual clients can tune.

## What

Two server-side changes in the worker's Pion WebRTC pipeline:

1. **FEC (forward error correction) on outbound video.** Adds
   redundancy packets so the receiver reconstructs burst losses
   without a retransmit round-trip. Directly patches the AWDL-dwell
   failure mode and any similar transient wireless blip.
2. **DSCP marking RTP packets as AF41** (video) **and EF** (voice).
   UniFi (and most consumer routers) map DSCP to WMM QoS queues;
   marked RTP wins priority over background traffic on a busy Wi-Fi.

Neither requires the client to do anything. Every user benefits
whether or not they know about AWDL or their network.

## How

### FEC

- Pion exposes both **ULPFEC** and **FlexFEC**. ULPFEC is older/wider
  compatibility; FlexFEC is newer and more efficient. Chrome
  (VideoToolbox receiver on Mac) supports both. Start with **ULPFEC**
  for simplicity — fewer SDP munging edge cases.
- Look in `pkg/worker/media/` (where `WebRtcMediaPipe` lives) for the
  peer-connection / interceptor setup. The interceptor registry is
  where the FEC interceptor gets added.
- Typical Pion pattern:

  ```go
  import "github.com/pion/interceptor/pkg/fec"

  fecGen, err := fec.NewFecInterceptor(
      fec.FECOption.Payloads(...), // FEC payload types from SDP
  )
  registry.Add(fecGen)
  ```

  (Exact API may differ — check current pion/interceptor version.)
- **Redundancy level**: start at ~20 % (1 FEC packet per 5 media
  packets). Tunable via config; add a knob under `encoder.video.fec.*`
  in `config.yaml` (`enabled: true`, `redundancy: 0.2`).
- **Bandwidth cost**: +20 % outbound. At 8 Mbps target that's ~10 Mbps
  on the wire. Fine for typical upstreams; user's link handles 15 Mbps
  already per earlier profile.

### DSCP

- Pion lets you set DSCP via `webrtc.SettingEngine.SetSCTPMaxReceiveBufferSize`
  — no wait, DSCP on media is via ICE/DTLS transport options. Look
  for `SetDSCP` or per-transceiver DSCP config in Pion API at our
  pinned version. If not exposed on the transceiver, set on the ICE
  UDPMux.
- Values to mark:
  - Video RTP: **AF41** (DSCP 34)
  - Audio RTP: **EF** (DSCP 46)
  - RTCP: same as the track they serve
- Verification: `tcpdump -nni <iface> -x udp and src host <worker-ip>`
  and read the DS byte (the 2nd byte of the IP header, right-shifted
  by 2 to get the 6-bit DSCP). Should show 0x88 (=34) for video RTP.

### Config surface

Add to `config.yaml` under `encoder.video`:

```yaml
encoder:
  video:
    fec:
      enabled: true
      redundancy: 0.2       # 0.0–0.5
    dscp:
      video: 34             # AF41
      audio: 46             # EF
```

Operator-customized on moon, defaulted to enabled in the repo version.

### Verification

1. **Before/after WebRTC dumps** from `chrome://webrtc-internals/`
   playing Power Stone 2 or Halo for 60 s each, Wi-Fi only.
   Compare `totalFreezesDuration` delta — target is < 0.5 s per 60 s
   (baseline was 1.53 s per 51 s).
2. **Inspect SDP** in the CDP harness or webrtc-internals for
   `a=rtpmap:... ULPFEC/...` lines and `a=fmtp:...` matching.
3. **tcpdump on moon** to confirm DSCP is set on egress.

### Rollback

All behind config flags; set `fec.enabled: false` or `dscp.video: 0`
to disable without rebuild. Config is restart-only, no container
rebuild.

## Files likely to touch

- `pkg/worker/media/webrtc_media_pipe.go` (or wherever the Pion
  `PeerConnection` / `InterceptorRegistry` is built — grep for
  `NewInterceptorRegistry` and `PeerConnection`)
- `pkg/config/worker.go` (new `FEC`, `DSCP` subtypes)
- `config/config.yaml` in the repo (defaults)
- `docs/perf-profile-2026-04-20.md` — add a "Followup landed" section
  once the diff ships, linking to this plan

## Don't-forgets

- Pion version on the tree at time of work — APIs around interceptor
  registration have churned between 3.x and 4.x. Match `go.mod`.
- DSCP marking over Linux unprivileged containers requires
  `CAP_NET_ADMIN` or `sysctl net.ipv4.ip_unprivileged_port_start`-style
  relaxations. If setting DSCP fails silently, check capability set
  on the quadlet.
- FEC increases CPU on both encoder and decoder. At 60 fps 640×480,
  negligible. At higher resolutions revisit.

## Out of scope for this plan (but adjacent)

- Playout delay hint (receiver buffer nudge).
- Adaptive GOP tightening after a PLI burst.
- Switching to SVC (scalable video coding) layers for graceful
  degradation under heavy loss.

These are follow-ups if FEC + DSCP don't close the gap.

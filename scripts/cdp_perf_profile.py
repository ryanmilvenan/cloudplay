#!/usr/bin/env python3
"""cdp_perf_profile.py — time-series WebRTC perf profile for a cloudplay
session via the CDP Chrome at 127.0.0.1:9222.

Launches a specific game in the host tab, waits for the stream to start,
then polls RTCPeerConnection.getStats() at a fixed cadence for the
requested duration. Writes a CSV of per-sample inbound-rtp video metrics
plus a summary with the deltas that tell you where the frame drops
happen.

Usage:
    scripts/cdp_perf_profile.py "Power Stone 2"              # 120 s default
    scripts/cdp_perf_profile.py "Halo" --seconds 180         # longer run
    scripts/cdp_perf_profile.py "Power Stone 2" --out ps2.csv

Prereqs: same dedicated CDP Chrome setup as cdp_multiuser_driver.py —
127.0.0.1:9222 with https://games-dev.milvenan.technology/ open.

Output:
  <stem>.csv          — per-sample raw counters and smoothed metrics
  <stem>.summary.txt  — one-line-per-metric summary (mean, p50/p95, worst)

What we're hunting: a rhythmic frame-drop pattern. The summary highlights
periodic dips by reporting the autocorrelation peak of the drop rate
time series — a strong peak at some lag N suggests something happens
every N seconds (e.g. a background GC, a flush, a bitrate ratchet).
"""
import argparse, json, sys, time, urllib.request, socket, base64, os, statistics
from urllib.parse import urlparse

CDP = "http://127.0.0.1:9222"
HOST = "games-dev.milvenan.technology"


# ── CDP plumbing (same pattern as cdp_multiuser_driver.py) ──────────────
def list_tabs():
    return json.loads(urllib.request.urlopen(CDP + "/json").read())


def find_tab(host):
    for t in list_tabs():
        if host in t.get("url", ""):
            return t
    raise SystemExit(f"No tab for {host}. Open it in CDP Chrome first.")


class Tab:
    def __init__(self, host):
        self.host = host
        meta = find_tab(host)
        u = urlparse(meta["webSocketDebuggerUrl"])
        s = socket.create_connection((u.hostname, u.port), timeout=10)
        key = base64.b64encode(os.urandom(16)).decode()
        req = (
            f"GET {u.path} HTTP/1.1\r\nHost: {u.hostname}:{u.port}\r\n"
            f"Upgrade: websocket\r\nConnection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n"
        )
        s.sendall(req.encode())
        buf = b""
        while b"\r\n\r\n" not in buf:
            buf += s.recv(4096)
        if b"101" not in buf.split(b"\r\n", 1)[0]:
            raise SystemExit(f"CDP handshake failed: {buf[:200]}")
        import websocket
        self.ws = websocket.WebSocket()
        self.ws.connect(meta["webSocketDebuggerUrl"], suppress_origin=True)
        self.id = 0

    def send(self, method, params=None):
        self.id += 1
        self.ws.send(json.dumps({"id": self.id, "method": method, "params": params or {}}))
        while True:
            msg = json.loads(self.ws.recv())
            if msg.get("id") == self.id:
                return msg

    def eval(self, expr, await_promise=False):
        r = self.send("Runtime.evaluate", {
            "expression": expr,
            "returnByValue": True,
            "awaitPromise": await_promise,
        })
        res = r.get("result", {}).get("result", {})
        if res.get("subtype") == "error":
            return ("ERROR", res.get("description", ""))
        return ("OK", res.get("value"))


# ── Browser-side data collection ────────────────────────────────────────
def click_game(tab, pattern):
    return tab.eval(f"""(() => {{
        const input = document.getElementById('game-select-input');
        if (!input) return 'no-input';
        // Flip AI mode off so the fuzzy dropdown is the search target.
        const toggle = document.getElementById('game-select-ai-toggle');
        if (toggle && toggle.classList.contains('is-on')) toggle.click();
        input.focus();
        input.value = {json.dumps(pattern)};
        input.dispatchEvent(new Event('input', {{bubbles:true}}));
        // Give the debounced semantic filter a tick to settle.
        return new Promise((res) => setTimeout(() => {{
            const hits = Array.from(document.querySelectorAll('.game-select__result'));
            if (!hits.length) return res('no-hits');
            hits[0].click();
            res('clicked: ' + (hits[0].textContent || '').slice(0, 80));
        }}, 400));
    }})()""", await_promise=True)


def stream_ready(tab):
    """Returns True once the <video> element has a positive currentTime."""
    _, v = tab.eval(
        "document.querySelector('#stream')?.currentTime || 0"
    )
    try:
        return float(v or 0) > 0.1
    except (TypeError, ValueError):
        return False


# Keys we want from the RTCPeerConnection.getStats() report. Chrome's
# inbound-rtp (kind=video) is the authoritative decode-side view.
VIDEO_FIELDS = [
    "framesReceived", "framesDecoded", "framesDropped",
    "bytesReceived", "packetsReceived", "packetsLost",
    "nackCount", "pliCount", "firCount",
    "keyFramesDecoded", "framesPerSecond", "framesAssembledFromMultiplePackets",
    "jitter", "jitterBufferDelay", "jitterBufferEmittedCount",
    "totalDecodeTime", "totalInterFrameDelay", "totalSquaredInterFrameDelay",
    "freezeCount", "totalFreezesDuration",
    "pauseCount", "totalPausesDuration",
    "qpSum",
]


def getstats_snapshot(tab):
    """Returns the inbound-rtp video stats dict plus wallclock timestamp."""
    url_script = tab.eval(
        "Array.from(document.querySelectorAll('script[src*=\"?v=\"]'))"
        ".map(s => (s.src.match(/\\?v=([a-f0-9v0-9]+)/)||[])[1]).find(Boolean) || ''"
    )
    stamp = url_script[1] or ""
    q = f"?v={stamp}" if stamp else ""
    url = f"/js/network/webrtc.js{q}"
    fields_js = json.dumps(VIDEO_FIELDS)
    # statsRaw bypasses the connected-flag guard; use stats as fallback
    # for older deploys where statsRaw isn't shipped yet.
    code = f"""(async () => {{
        const mod = await import('{url}');
        const fn = mod.webrtc.statsRaw || mod.webrtc.stats;
        const report = await fn();
        if (!report) return null;
        const want = {fields_js};
        const pick = {{}};
        report.forEach(s => {{
            if (s.type === 'inbound-rtp' && s.kind === 'video') {{
                for (const k of want) if (k in s) pick[k] = s[k];
                pick.ts = s.timestamp;
                pick.decoderImpl = s.decoderImplementation || '';
                pick.ssrc = s.ssrc;
            }}
        }});
        return pick;
    }})()"""
    status, val = tab.eval(code, await_promise=True)
    return val if status == "OK" else None


def force_play_muted(tab):
    """Bypass Chrome's autoplay policy so the CDP-driven session actually
    renders frames. Without this, the stream buffers but stays paused."""
    return tab.eval("""(async () => {
        const v = document.querySelector('#stream');
        if (!v) return 'no-stream';
        v.muted = true;
        try { await v.play(); return 'playing'; }
        catch (e) { return 'play-failed: ' + e.message; }
    })()""", await_promise=True)


# ── Sampling loop + CSV write ───────────────────────────────────────────
def run(game, seconds, out_path, poll_hz, reload):
    print(f"[perf] connecting to CDP at {CDP}", flush=True)
    tab = Tab(HOST)
    # Chrome throttles hidden tabs aggressively (RAF 1fps, video decode
    # throttling, timers clamped to ≥1s). That inflates the "packet
    # loss" and "freeze" metrics into artifact if we don't foreground
    # the tab first. Page.bringToFront puts the CDP tab into a visible
    # state without needing a real user gesture.
    tab.send("Page.enable")
    front = tab.send("Page.bringToFront")
    if front.get("error"):
        print(f"[perf] bringToFront warning: {front['error']}", flush=True)
    if reload:
        print("[perf] reloading tab for clean state", flush=True)
        tab.send("Page.reload", {"ignoreCache": True})
        # Wait for the library broadcast and menu render.
        time.sleep(6)
        tab.send("Page.bringToFront")  # reloaded tab may have lost foreground
    print(f"[perf] clicking '{game}'", flush=True)
    click_res = click_game(tab, game)
    print(f"[perf] click_game → {click_res}", flush=True)

    # Wait for stream to start playing. Hydration + launch takes a few
    # seconds; big archives take longer. Cap at 90 s so a broken launch
    # doesn't leave the script hanging forever.
    print("[perf] waiting for stream…", flush=True)
    deadline = time.time() + 90
    ready = False
    while time.time() < deadline:
        if stream_ready(tab):
            ready = True
            break
        time.sleep(0.5)
    if not ready:
        # Chrome's autoplay policy keeps the video paused when no user
        # gesture has occurred on the page; force-mute and play().
        # Stream may be buffering just fine, we just can't see it advance.
        print("[perf] stream paused by autoplay; forcing muted play", flush=True)
        print(f"[perf] force_play → {force_play_muted(tab)}", flush=True)
        deadline = time.time() + 30
        while time.time() < deadline:
            if stream_ready(tab):
                ready = True
                break
            time.sleep(0.5)
    if not ready:
        print("[perf] stream never started; aborting", file=sys.stderr)
        sys.exit(1)
    print("[perf] stream is live; sampling", flush=True)

    period = 1.0 / poll_hz
    samples = []
    start = time.time()
    next_sample = start
    while time.time() - start < seconds:
        snap = getstats_snapshot(tab)
        if snap:
            samples.append({"wall": time.time() - start, **snap})
        next_sample += period
        sleep_for = next_sample - time.time()
        if sleep_for > 0:
            time.sleep(sleep_for)
        else:
            # Fell behind — reset cadence so we don't burst.
            next_sample = time.time()

    print(f"[perf] collected {len(samples)} samples", flush=True)

    # Write CSV. Counters are cumulative; we write both raw and per-sample
    # deltas so the consumer doesn't have to diff by hand.
    cols = ["wall"] + VIDEO_FIELDS + ["ts", "decoderImpl", "ssrc"]
    delta_cols = [c + "_d" for c in VIDEO_FIELDS if c not in ("framesPerSecond", "jitter", "jitterBufferDelay", "qpSum")]
    with open(out_path, "w") as f:
        f.write(",".join(cols + delta_cols) + "\n")
        prev = None
        for s in samples:
            row = [_csv(s.get(c)) for c in cols]
            deltas = []
            for c in delta_cols:
                base = c[:-2]
                if prev is None:
                    deltas.append("")
                else:
                    try:
                        deltas.append(str(float(s.get(base, 0)) - float(prev.get(base, 0))))
                    except (TypeError, ValueError):
                        deltas.append("")
            f.write(",".join(row + deltas) + "\n")
            prev = s
    print(f"[perf] wrote {out_path}", flush=True)

    # Summary
    summary_path = os.path.splitext(out_path)[0] + ".summary.txt"
    write_summary(samples, summary_path, poll_hz)
    print(f"[perf] wrote {summary_path}", flush=True)


def _csv(v):
    if v is None:
        return ""
    if isinstance(v, float):
        return f"{v:.6g}"
    return str(v)


# ── Summarization: means, percentiles, autocorrelation ──────────────────
def write_summary(samples, path, poll_hz):
    if len(samples) < 5:
        with open(path, "w") as f:
            f.write(f"too few samples ({len(samples)})\n")
        return

    def deltas(field):
        out = []
        for i in range(1, len(samples)):
            a = samples[i - 1].get(field)
            b = samples[i].get(field)
            if a is None or b is None:
                continue
            try:
                out.append(float(b) - float(a))
            except (TypeError, ValueError):
                continue
        return out

    frames_recv = deltas("framesReceived")
    frames_dec = deltas("framesDecoded")
    frames_drop = deltas("framesDropped")
    bytes_recv = deltas("bytesReceived")
    packets_lost = deltas("packetsLost")
    nacks = deltas("nackCount")
    plis = deltas("pliCount")
    firs = deltas("firCount")
    freezes = deltas("freezeCount")
    freeze_dur = deltas("totalFreezesDuration")
    decode_time = deltas("totalDecodeTime")
    interframe = deltas("totalInterFrameDelay")

    def stats(name, xs, unit=""):
        if not xs:
            return f"{name:34s} (no data)"
        mean = statistics.mean(xs)
        p50 = statistics.median(xs)
        p95 = _percentile(xs, 95)
        worst = max(xs)
        return f"{name:34s} mean={mean:.3f} p50={p50:.3f} p95={p95:.3f} worst={worst:.3f} {unit}".rstrip()

    lines = []
    lines.append(f"samples={len(samples)}  duration={samples[-1]['wall']:.1f}s  poll_hz={poll_hz}")
    lines.append(f"decoder={samples[-1].get('decoderImpl','?')}  ssrc={samples[-1].get('ssrc','?')}")
    lines.append("")
    lines.append("per-sample deltas (one sample = " + f"{1.0/poll_hz:.2f}s" + "):")
    lines.append("  " + stats("frames received", frames_recv, "frames"))
    lines.append("  " + stats("frames decoded", frames_dec, "frames"))
    lines.append("  " + stats("frames dropped", frames_drop, "frames"))
    lines.append("  " + stats("bytes received", bytes_recv, "B"))
    lines.append("  " + stats("packets lost", packets_lost, "pkts"))
    lines.append("  " + stats("NACKs sent", nacks))
    lines.append("  " + stats("PLIs sent", plis))
    lines.append("  " + stats("FIRs sent", firs))
    lines.append("  " + stats("freeze count", freezes))
    lines.append("  " + stats("freeze duration", freeze_dur, "s"))
    lines.append("  " + stats("decode-time delta", decode_time, "s"))
    lines.append("  " + stats("inter-frame delay delta", interframe, "s"))
    lines.append("")
    # Instantaneous framesPerSecond (already a rate from Chrome)
    fps_series = [s.get("framesPerSecond") for s in samples if s.get("framesPerSecond") is not None]
    jitter_series = [s.get("jitter") for s in samples if s.get("jitter") is not None]
    if fps_series:
        lines.append(f"  framesPerSecond mean={statistics.mean(fps_series):.2f} p50={statistics.median(fps_series):.2f} min={min(fps_series):.2f}")
    if jitter_series:
        lines.append(f"  jitter (s)      mean={statistics.mean(jitter_series):.4f} p95={_percentile(jitter_series, 95):.4f} max={max(jitter_series):.4f}")
    lines.append("")

    # Autocorrelation of frames_dropped delta — finds the rhythmic pattern.
    ac = _autocorr(frames_drop, max_lag=min(30, len(frames_drop) // 3))
    if ac:
        lines.append("frames-dropped autocorrelation (lag × sample_period seconds):")
        for lag, v in ac:
            bar = "#" * max(1, int(v * 40))
            lines.append(f"  lag {lag:3d} ({lag/poll_hz:5.2f}s)  corr={v:+.3f}  {bar}")
        peak_lag, peak_v = max(ac[1:], key=lambda x: x[1], default=(0, 0))
        if peak_v > 0.25:
            lines.append(f"  → rhythmic drops suspected at ~{peak_lag/poll_hz:.2f}s period (r={peak_v:.2f})")
        else:
            lines.append("  → no strong periodic signal")

    with open(path, "w") as f:
        f.write("\n".join(lines) + "\n")


def _percentile(xs, p):
    xs_sorted = sorted(xs)
    k = (len(xs_sorted) - 1) * (p / 100)
    f = int(k)
    c = min(f + 1, len(xs_sorted) - 1)
    if f == c:
        return xs_sorted[f]
    return xs_sorted[f] + (xs_sorted[c] - xs_sorted[f]) * (k - f)


def _autocorr(xs, max_lag=20):
    """Pearson autocorrelation at lags 1..max_lag. Returns [(lag, corr), …]."""
    if len(xs) < max_lag + 2:
        return []
    mean = statistics.mean(xs)
    denom = sum((x - mean) ** 2 for x in xs) or 1.0
    out = []
    for lag in range(1, max_lag + 1):
        num = sum((xs[i] - mean) * (xs[i + lag] - mean) for i in range(len(xs) - lag))
        out.append((lag, num / denom))
    return out


# ── Entry ───────────────────────────────────────────────────────────────
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("game", help="substring matched against the search bar")
    ap.add_argument("--seconds", type=int, default=120, help="sampling duration")
    ap.add_argument("--hz", type=float, default=2.0, help="samples per second")
    ap.add_argument("--out", default=None, help="CSV output path")
    ap.add_argument("--no-reload", action="store_true", help="skip the tab reload (use when the menu is already fresh)")
    args = ap.parse_args()

    if args.out is None:
        slug = "".join(c if c.isalnum() else "-" for c in args.game).strip("-").lower()
        args.out = f"perf-{slug}.csv"

    run(args.game, args.seconds, args.out, args.hz, reload=not args.no_reload)


if __name__ == "__main__":
    main()

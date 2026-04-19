# ROM storage on the NAS — optimization options

Context: moon's NFS mount to the Synology NAS at `192.168.1.100:/volume1/data`
(mount point `/var/mnt/data`) is the intended home for the large ROM library
(every Xbox game ever made, long-tail PS2/GC/Wii titles, etc.). The local
`~/containers/cloudplay/games/` tree stays as an override / staging area;
the bind mount into the container is swapped to point at the NAS for the
systems that live there.

Goal of this doc: capture optimizations that make gameplay snappy even with
ROMs served over a network filesystem, so we have a reference when we decide
to act on them. None of this is implemented yet.

## The intended layout

We want the NAS library to mirror the local structure one-for-one, so the
cloudplay library watcher and config schema don't have to learn about
tiered storage. Under `/var/mnt/data/media/games/`:

```
/var/mnt/data/media/games/
  xbox/       <- .iso / .xiso files
  ps2/        <- .iso / .chd / .cso / .bin / .cue
  gc/         <- .iso / .nkit.iso / .gcm / .gcz / .rvz
  wii/        <- .wbfs / .wia / .rvz / .iso
  dreamcast/
  n64/
  psx/        <- config.yaml calls the system 'pcsx' but the folder is 'psx'
  ...
```

The cloudplay quadlet then bind-mounts that dir over the container's
`assets/games` — same path, same file layout, same watcher behavior. Swap one line in the quadlet:

```ini
# before
Volume=/home/rocks/containers/cloudplay/games:/usr/local/share/cloud-game/assets/games:ro,z

# after
Volume=/var/mnt/data/media/games:/usr/local/share/cloud-game/assets/games:ro,z
```

Keep the local `~/containers/cloudplay/games/` tree around as a staging
area — stuff you're testing before uploading to the NAS, save states,
anything you don't want globally visible. Could be overlay-mounted on top
of the NAS mount so both are visible to the container at once, but that
adds a layer of complexity that's not worth it until we hit the need.

## Baseline cost

- Link: moon has a 1GbE NIC (cap ≈110 MB/s) to the NAS.
- NFS mount options today: NFSv4.1, `rsize=wsize=131072` (128 KB per RPC).
- Disc-era systems do streaming reads during gameplay; throughput is never
  the bottleneck at 1GbE, but per-read latency and RPC round-trips can be.
- Page cache: moon has 128 GB of RAM; once an ISO has been read end-to-end
  anything else would need to evict ~100 GB to push it out. In practice a
  played game stays hot across sessions.

## Layer 1 — NFS mount params (low effort, pure win)

The adjacent `xteve` mount uses the modern defaults the `/var/mnt/data`
mount doesn't. Copy them.

```ini
# /etc/systemd/system/var-mnt-data.mount
[Unit]
Description=NFS Share — Synology data volume
After=network-online.target
Wants=network-online.target

[Mount]
What=192.168.1.100:/volume1/data
Where=/var/mnt/data
Type=nfs4
Options=vers=4.2,rsize=1048576,wsize=1048576,hard,proto=tcp,timeo=600,_netdev

[Install]
WantedBy=multi-user.target
```

Apply:
```
sudo systemctl daemon-reload
sudo umount /var/mnt/data
sudo systemctl start var-mnt-data.mount
mount | grep data   # verify rsize=1048576
```

Expected: 2–4× read throughput on cold reads, less round-trip pressure
during gameplay streaming.

## Layer 2 — Page-cache warm-up at session start (biggest gameplay win)

Before `c.proc.Start()` in `pkg/worker/caged/xemu/process.go`, read the
active ISO end-to-end so the kernel pulls it into the page cache. xemu's
DVD reads during gameplay then hit RAM, not the network.

Sketch:

```go
// Prewarm the ISO's contents into the kernel page cache before xemu opens
// it as a DVD. Turns the first few seconds of network pressure into a
// one-shot streaming fetch and makes subsequent session reads RAM-speed.
// With moon's 128 GB RAM this cache is effectively permanent across
// sessions — only an eviction storm from other reads can clear it.
func prewarm(path string, log *logger.Logger) {
    f, err := os.Open(path)
    if err != nil { return }
    defer f.Close()
    n, _ := io.Copy(io.Discard, f)
    log.Info().Str("iso", filepath.Base(path)).
        Int64("bytes", n).Msg("[XEMU-PROC] ISO page-cache prewarm done")
}
```

Call it from `Process.Start` right after `writeConfig`, gated by a new
`XemuConfig.IsoPrewarm bool` field (default true once shipped).

Cost: ~30 s of "loading…" on first play of any given ISO over 1GbE.
Subsequent plays: instant. No persistence — first play after reboot pays
the cost again.

Nice side effect: the prewarm is also a nice integration-test probe —
fail loudly if the ISO path is broken, before xemu's less-legible error
appears.

## Layer 3 — cachefilesd / FS-Cache (persistent, automatic)

NFS-level block cache backed by local SSD. First NAS read populates the
cache; subsequent reads come off NVMe (~1 ms latency, matches layer 2 in
practice). Survives reboots. More ops setup than Layer 2.

```
sudo dnf install cachefilesd            # Fedora/bluefin
sudo systemctl enable --now cachefilesd
```

Update mount options to add `fsc`:

```
Options=vers=4.2,rsize=1048576,wsize=1048576,hard,proto=tcp,timeo=600,_netdev,fsc
```

Give it a disk budget (say 500 GB of the NVMe) in
`/etc/cachefilesd.conf`:

```
dir /var/cache/fscache
tag mycache
brun 10%
bcull 7%
bstop 3%
frun 10%
fcull 7%
fstop 3%
```

Benefit over layer 2: crossing a container restart / machine reboot keeps
the cache warm, so the next player session is instant too.

## Layer 4 — tmpfs stage (edge case)

For single-title marathon sessions on large PS2 ISOs (e.g. GTA:SA, SotC,
Shadow of the Colossus) where even page-cached reads show the occasional
hitch on texture streaming, copy the ISO to `/dev/shm` and point xemu /
PCSX2 at the tmpfs path. Avoids any possibility of page eviction.

Cost: full ISO worth of RAM, pinned. Fine at 128 GB for any single game.
Not worth building until a specific title demands it.

## Recommended order if we ever do this

1. Layer 1 (mount params) — one systemd edit, applies immediately.
2. Layer 2 (prewarm) — small code change, big gameplay impact, minimal
   ops surface.
3. Layer 3 (cachefilesd) — only if we want persistence across
   reboots.
4. Layer 4 — skip unless a specific title forces it.

## Why this file exists

We chose to keep the ROM library on the NAS for consolidation, but the
xemu native backend does random-access reads on the emulated DVD during
gameplay. Without any of the above, a large-ISO title boots fine but may
stutter on texture streaming over the network. This doc is the playbook
for when we decide to eliminate that variable.

# Enabling the xemu (Xbox) backend in production

xemu is installed in the cloudplay container as of Phase 7 but off by
default (`xemu.enabled: false` in `config.yaml`). This runbook takes a
fresh moon deployment to the point where xbox games play end-to-end.

Prerequisites: you have shell access to moon, you can sudo there, the
cloudplay container is already running libretro games fine, and you have
legitimate copies of the Xbox BIOS files.

## 1. Populate BIOS files on moon

xemu is legally prohibited from shipping BIOS dumps, so each operator
provides their own. Put them here:

```
~/containers/cloudplay/xemu-bios/
├── bios/
│   └── <flash-bios>.bin      # e.g. Complex_4627.bin (1 MB or 256 KB)
├── hdd/
│   └── xbox_hdd.qcow2        # writable; xemu rewrites save games here
└── mcpx/
    └── mcpx_1.0.bin          # 512 bytes, SHA1 5d270675... or 890cd3c2...
```

xemu's `process.go` globs the subdirectories for `*.bin` / `*.qcow2`,
so exact filenames don't matter.

## 2. Grant uinput access

The xemu backend's virtual gamepad needs `/dev/uinput` writable by the
rootless container's user. Install the shipped udev rule once:

```bash
sudo cp ~/containers/cloudplay/systemd/99-cloudplay-uinput.rules \
        /etc/udev/rules.d/
sudo udevadm control --reload
# On some systems uinput is a module; on others it's built in. Try both:
sudo modprobe -r uinput 2>/dev/null
sudo modprobe uinput   2>/dev/null || true
# If the rule didn't fire because uinput is built in, reset the node directly:
sudo chgrp input /dev/uinput 2>/dev/null
sudo chmod 0660 /dev/uinput 2>/dev/null
# Give the rocks user input-group membership so the rootless namespace
# can propagate it via keep-groups:
sudo usermod -aG input rocks   # then log out and back in
```

Verify: `ls -la /dev/uinput` should show `root input 0660` (or
`root root 0660` with an ACL granting rocks rw — either works if the
container can open it).

On rocks's `/etc/subgid` the input gid (typically 104) must be in a
mapped range for `--group-add keep-groups` to propagate. If it isn't,
`sudo chown rocks:rocks /dev/uinput` is a simpler unblock that survives
one boot — rerun after each reboot until the subgid mapping is in place.

## 3. Turn on xemu in config.yaml

Edit `~/containers/cloudplay/config/config.yaml`:

```yaml
xemu:
  enabled: true
  binaryPath: /usr/local/bin/xemu
  biosPath: /xemu-bios
  xvfbDisplay: ':100'
  width: 640
  height: 480
  audioCapture: true
  inputInject: true
```

Video capture (`ffmpeg -f x11grab`) is always on when the cage starts —
there's no toggle; the capture runs only while a game is loaded.

## 4. Drop an Xbox game in

Xbox discs must be in **xiso** format. If you have a redump-style ISO,
convert it first:

```bash
# Inside the dev container, where extract-xiso is built at /out/:
cp "/games/<game>.iso" "/games/<game>-xiso.iso"
/out/extract-xiso -r "/games/<game>-xiso.iso"
# Delete the .old backup when you're confident the rewrite succeeded.
```

Place the xiso in `~/containers/cloudplay/games/xbox/`. Trigger a rescan
(a noop touch in the top-level games dir fires the fsnotify watcher):

```bash
touch ~/containers/cloudplay/games/.poke && \
  rm ~/containers/cloudplay/games/.poke
```

Check the cloudplay log — you should see
`ps2 → xbox  Halo - Combat Evolved (USA) (Rev 2).xiso` style lines
with the right system classification.

## 5. Restart and verify

```bash
systemctl --user restart cloudplay
podman logs --since 1m cloudplay 2>&1 | \
  grep -iE 'xemu|backend|lib scan'
```

Expected log lines:

- `[XEMU-CAGE] registered (stub — Phase 1)` — backend wired up
- `Lib scan... completed`
- (on session start) `New room ... backend=xemu game=Halo...`
- `[XEMU-VIDEO] ffmpeg x11grab spawned`
- `[XEMU-AUDIO] target stream located app=xemu`
- `[XEMU-INPUT] virtual pad created`

## 6. Play

From `games.milvenan.technology`, the Xbox game appears in the library
with `backend: xemu`. Selecting it spawns xemu inside the container,
grabs frames into the existing NVENC → WebRTC pipeline, routes the
browser gamepad to a uinput-backed Xbox 360 controller, and pipes
audio through parec into the Opus encoder.

Save states / recording / cloud saves are **not supported** for the
xemu backend today; the handlers return `ErrPacket` for those requests.
The frontend already tolerates that and greys the buttons.

## Rolling back

```yaml
xemu:
  enabled: false
```

`systemctl --user restart cloudplay`. Libretro games continue unaffected
— none of the code paths this feature added are reachable with xemu off.

## Troubleshooting

- **"couldn't cage xemu: xemu backend disabled in config"** — expected
  when `enabled: false`; not an error.
- **"virtualpad: open /dev/uinput: permission denied"** — step 2 didn't
  land. Check `ls -la /dev/uinput` and `groups rocks`.
- **xemu shows "Please insert an Xbox disc..." despite game being
  selected** — the ISO isn't xiso format. Run `extract-xiso -r` first.
- **xemu is silent through the attract videos** — known xemu limitation
  with Bink video audio (Halo intro). In-game audio works.
- **Frames show the xemu "Guest has not initialized" overlay** — the
  capture path regressed. Check `docs/capture-path-not-taken-ld-preload.md`
  for the multi-window SDL story; we switched to x11grab in Phase 3b
  specifically to avoid this.

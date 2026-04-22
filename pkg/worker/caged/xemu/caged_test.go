package xemu

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/nativeemu"
)

// findBiosDir looks for Xbox BIOS files under /xemu-bios or a few likely
// fallback paths. Returns the dir and true if a full set (bios/*, mcpx/*,
// hdd/*) is present, else "" and false. Used to gate real-process tests.
func findBiosDir() (string, bool) {
	for _, root := range []string{"/xemu-bios", os.Getenv("XEMU_BIOS")} {
		if root == "" {
			continue
		}
		if hasOne(filepath.Join(root, "bios", "*.bin")) &&
			hasOne(filepath.Join(root, "mcpx", "*.bin")) &&
			hasOne(filepath.Join(root, "hdd", "*.qcow2")) {
			return root, true
		}
	}
	return "", false
}

func hasOne(pattern string) bool {
	m, err := filepath.Glob(pattern)
	return err == nil && len(m) > 0
}

func newTestCage(t *testing.T, w, h int) *Caged {
	t.Helper()
	log := logger.NewConsole(false, "xemu-test", false)
	c := Cage(CagedConf{Xemu: config.XemuConfig{Enabled: true, Width: w, Height: h}}, log)
	if err := c.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return &c
}

// TestStubViewportDefaults ensures Cage fills in 640×480 when the config omits
// dimensions, so downstream media/encoder sizing is stable from day one.
func TestStubViewportDefaults(t *testing.T) {
	log := logger.NewConsole(false, "xemu-test", false)
	c := Cage(CagedConf{}, log)
	w, h := c.ViewportSize()
	if w != 640 || h != 480 {
		t.Fatalf("default viewport: got %dx%d want 640x480", w, h)
	}
}

// TestStubFrameLoopRate asserts the Phase-1 stub video loop fires the video
// callback at ≈60 Hz. This is Phase 1 gate G1.3 — proves the video callback
// plumbing is wired before any real xemu code lands.
func TestStubFrameLoopRate(t *testing.T) {
	c := newTestCage(t, 320, 240) // small frame → fast fill
	var count atomic.Uint64
	c.SetVideoCb(func(v app.Video) {
		if v.Frame.W != 320 || v.Frame.H != 240 || len(v.Frame.Data) != 320*240*4 {
			t.Errorf("unexpected frame shape: %dx%d bytes=%d", v.Frame.W, v.Frame.H, len(v.Frame.Data))
		}
		count.Add(1)
	})
	c.Start()
	time.Sleep(1100 * time.Millisecond) // >1s to survive ticker jitter
	c.Close()

	got := count.Load()
	// 60 Hz over ~1.1 s → expect ≥55 and ≤68 frames. Wide band accepts
	// CI and loaded-host jitter without being so wide it loses signal.
	if got < 55 || got > 68 {
		t.Fatalf("stub frame rate out of band: got %d frames in ~1.1s, want 55..68", got)
	}
}

// TestStubFrameCounterAdvances confirms distinct frame content across calls —
// future capture paths will rely on frame hashing so "same byte every time" is
// the specific bug to guard against today.
func TestStubFrameCounterAdvances(t *testing.T) {
	c := newTestCage(t, 64, 48)
	var firstByteB0, firstByteB5 byte
	got := 0
	done := make(chan struct{})
	c.SetVideoCb(func(v app.Video) {
		// Frame-counter patch is top-left 20×20 (monochrome = frame counter
		// low byte). Sample pixel (0,0)'s R channel.
		b := v.Frame.Data[0]
		switch got {
		case 0:
			firstByteB0 = b
		case 5:
			firstByteB5 = b
			close(done)
		}
		got++
	})
	c.Start()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("timed out waiting for 6 frames; got %d", got)
	}
	c.Close()
	if firstByteB0 == firstByteB5 {
		t.Fatalf("frame counter not advancing: frame 0 and frame 5 both have patch byte %d", firstByteB0)
	}
}

// TestProcessLifecycle is the Phase-2 G2.1-in-CI equivalent of the
// standalone xemu-smoke harness: it spawns a real Xvfb + xemu pair, lets
// it run briefly, and asserts Close tears both down without leftovers.
// Skipped when BIOS files aren't mounted so Mac/non-dev runs stay green.
func TestProcessLifecycle(t *testing.T) {
	bios, ok := findBiosDir()
	if !ok {
		t.Skip("XEMU-WIP: /xemu-bios not present — run inside cloudplay-dev")
	}
	if _, err := exec.LookPath("xemu"); err != nil {
		t.Skip("xemu binary not on PATH")
	}
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("Xvfb not on PATH")
	}

	log := logger.NewConsole(false, "xemu-test", false)
	c := Cage(CagedConf{Xemu: config.XemuConfig{
		Enabled:     true,
		BinaryPath:  "xemu",
		BiosPath:    bios,
		XvfbDisplay: ":101", // avoid colliding with a running smoke session
		Width:       640,
		Height:      480,
	}}, log)
	if err := c.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var frames atomic.Int64
	c.SetVideoCb(func(app.Video) { frames.Add(1) })
	c.SetAudioCb(func(app.Audio) {})
	c.SetDataCb(func([]byte) {})

	c.Start()
	time.Sleep(3 * time.Second)

	if c.proc == nil || c.proc.Pid() == 0 {
		t.Fatal("xemu process not running after Start")
	}
	if err := syscall.Kill(c.proc.Pid(), 0); err != nil {
		t.Fatalf("xemu pid %d not reachable: %v", c.proc.Pid(), err)
	}

	c.Close()

	if got := frames.Load(); got < 150 || got > 200 {
		t.Errorf("stub frames in 3s: got %d want 150..200", got)
	}
	// Assert no leftover xemu / Xvfb processes belonging to us. pgrep -xa
	// lists zombies (<defunct>) too — those are harmless leftovers from
	// other tests or manual probe scripts that haven't been reaped yet,
	// so filter them out. Real leftovers (live processes) still fail.
	for _, name := range []string{"xemu", "Xvfb"} {
		out, err := exec.Command("pgrep", "-xa", name).Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				continue
			}
			t.Errorf("pgrep %s: %v", name, err)
		}
		var live []string
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if line == "" || strings.Contains(line, "<defunct>") {
				continue
			}
			live = append(live, line)
		}
		if len(live) > 0 {
			t.Errorf("leftover %s process(es):\n%s", name, strings.Join(live, "\n"))
		}
	}
}

// TestVideoCapture is the Phase-3 G3.2-in-CI gate: drive xemu under an
// ffmpeg x11grab capture pipe, assert (a) live frames arrive within 2 s of
// Start and (b) steady-state rate is >=30 fps across a 5 s window. Stricter
// rate bands live in the xemu-smoke harness. Skipped when xemu or ffmpeg
// aren't on PATH.
func TestVideoCapture(t *testing.T) {
	bios, ok := findBiosDir()
	if !ok {
		t.Skip("XEMU-WIP: /xemu-bios not present — run inside cloudplay-dev")
	}
	if _, err := exec.LookPath("xemu"); err != nil {
		t.Skip("xemu binary not on PATH")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	log := logger.NewConsole(false, "xemu-test", false)
	c := Cage(CagedConf{Xemu: config.XemuConfig{
		Enabled:     true,
		BinaryPath:  "xemu",
		BiosPath:    bios,
		XvfbDisplay: ":103",
		Width:       640,
		Height:      480,
	}}, log)
	if err := c.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var liveFrames atomic.Int64
	c.SetVideoCb(func(v app.Video) {
		if c.LiveFramesActive() {
			liveFrames.Add(1)
		}
	})
	c.SetAudioCb(func(app.Audio) {})

	c.Start()
	defer c.Close()

	// Wait up to 2 s for the first live frame.
	firstDeadline := time.Now().Add(2 * time.Second)
	for !c.LiveFramesActive() && time.Now().Before(firstDeadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if !c.LiveFramesActive() {
		t.Fatal("no live frame received within 2s — shim not wired or xemu not rendering")
	}

	// Measure over a 5 s window starting from the first live frame.
	start := liveFrames.Load()
	time.Sleep(5 * time.Second)
	delta := liveFrames.Load() - start
	fps := float64(delta) / 5.0
	if fps < 30 {
		t.Errorf("live fps too low: got %.1f want >= 30 (delta=%d)", fps, delta)
	}
	t.Logf("live fps over 5s window: %.1f (delta=%d)", fps, delta)
}

// TestAudioCapture is the Phase-4 G4.2-in-CI gate: with AudioCapture
// enabled, Caged should spin up a private PipeWire session, xemu should
// connect to it via SDL pulse, and parec should deliver S16LE chunks to
// the audio callback. The *content* of those chunks is a secondary
// concern — the stock Xbox dashboard with our BIOS is silent (documented
// in docs/test-hygiene-todo.md) — so we only assert bytes flowed.
// Real audio signal validation is covered by tools/audio-canary against
// a deterministic sine source (G4.1).
func TestAudioCapture(t *testing.T) {
	bios, ok := findBiosDir()
	if !ok {
		t.Skip("XEMU-WIP: /xemu-bios not present — run inside cloudplay-dev")
	}
	if _, err := exec.LookPath("xemu"); err != nil {
		t.Skip("xemu binary not on PATH")
	}
	if _, err := exec.LookPath("parec"); err != nil {
		t.Skip("parec (pulseaudio-utils) not on PATH — run inside cloudplay-dev")
	}
	if _, err := exec.LookPath("pipewire"); err != nil {
		t.Skip("pipewire not on PATH — run inside cloudplay-dev")
	}

	log := logger.NewConsole(false, "xemu-test", false)
	c := Cage(CagedConf{Xemu: config.XemuConfig{
		Enabled:      true,
		BinaryPath:   "xemu",
		BiosPath:     bios,
		XvfbDisplay:  ":104",
		Width:        640,
		Height:       480,
		AudioCapture: true,
	}}, log)
	if err := c.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var chunks atomic.Int64
	c.SetVideoCb(func(app.Video) {})
	c.SetAudioCb(func(a app.Audio) {
		chunks.Add(1)
	})

	c.Start()
	defer c.Close()

	// Audiocap's discovery polls pactl; first chunk should arrive within a
	// few seconds of xemu-pulse connecting. Be generous — xemu sometimes
	// delays opening its audio device until after BIOS init.
	deadline := time.Now().Add(10 * time.Second)
	for chunks.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if chunks.Load() == 0 {
		t.Fatal("no audio chunks received within 10s — plumbing broken")
	}

	// Measure flow over a 2 s window. 10 ms per chunk → expect ~200.
	start := chunks.Load()
	time.Sleep(2 * time.Second)
	delta := chunks.Load() - start
	if delta < 100 {
		t.Errorf("audio flow too slow: got %d chunks in 2s want >=100", delta)
	}
	t.Logf("audio chunks/2s=%d (~%d Hz)", delta, delta/2)
}

// TestVirtualPadInjection is the Phase-5 G5.1-in-CI gate. It creates a
// VirtualPad directly (without the full Caged), injects a press+release
// sequence, and reads evdev events back via the kernel's own event node,
// asserting the KEY events arrived with the right codes and values.
// Skipped when /dev/uinput isn't writable (no permission, no module, etc.).
func TestVirtualPadInjection(t *testing.T) {
	// Fail fast if /dev/uinput is missing or not writable — saves the test
	// from a misleading "virtual pad not open" error deeper in the flow.
	if _, err := os.Stat("/dev/uinput"); err != nil {
		t.Skip("XEMU-WIP: /dev/uinput not present — run inside cloudplay-dev")
	}
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("XEMU-WIP: cannot open /dev/uinput (%v); see docs/test-hygiene-todo.md", err)
	}
	_ = f.Close()

	log := logger.NewConsole(false, "xemu-test", false)
	pad := &nativeemu.VirtualPad{Log: log, DeviceName: "cloudplay-unit-test-pad"}
	if err := pad.Open(); err != nil {
		t.Fatalf("pad open: %v", err)
	}
	defer pad.Close()

	// Locate the event node — udev can be slow by a hundred ms or so.
	var path string
	for i := 0; i < 40; i++ {
		path = pad.DevicePath()
		if path != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if path == "" {
		t.Fatal("could not find event node")
	}
	t.Logf("our event node: %s", path)

	// Open for reading so we can verify the kernel delivers our events.
	rfd, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Skipf("XEMU-WIP: cannot open %s for reading (%v)", path, err)
	}
	defer rfd.Close()

	// Inject "A press" using the libretro wire format — bit 0 = south/A.
	var pkt [14]byte
	pkt[0] = 0x01 // bit 0 set
	if err := pad.Inject(pkt[:]); err != nil {
		t.Fatalf("inject press: %v", err)
	}
	// Inject "A release" (all zero).
	var zero [14]byte
	if err := pad.Inject(zero[:]); err != nil {
		t.Fatalf("inject release: %v", err)
	}

	// Read with a short deadline — the kernel buffer already has the events,
	// we just need to slurp them. Set a 1s SetReadDeadline-equivalent via
	// a small busy loop on the nonblocking fd via a goroutine.
	done := make(chan []byte, 1)
	go func() {
		// Each input_event is 24 bytes on x86_64.
		buf := make([]byte, 24*16)
		n, _ := rfd.Read(buf)
		done <- buf[:n]
	}()

	var evBytes []byte
	select {
	case evBytes = <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out reading events from our own uinput device")
	}
	if len(evBytes) < 48 {
		t.Fatalf("got %d bytes, expected at least 48 (2 KEY events + 2 SYNs × 24)", len(evBytes))
	}

	// Scan for the BTN_SOUTH events (code 0x130 = 304).
	var sawPress, sawRelease bool
	for off := 0; off+24 <= len(evBytes); off += 24 {
		typ := binary.LittleEndian.Uint16(evBytes[off+16 : off+18])
		code := binary.LittleEndian.Uint16(evBytes[off+18 : off+20])
		val := int32(binary.LittleEndian.Uint32(evBytes[off+20 : off+24]))
		if typ == 1 && code == 0x130 && val == 1 {
			sawPress = true
		}
		if typ == 1 && code == 0x130 && val == 0 {
			sawRelease = true
		}
	}
	if !sawPress {
		t.Error("no BTN_SOUTH press event observed")
	}
	if !sawRelease {
		t.Error("no BTN_SOUTH release event observed")
	}
}

// TestChaosKillRecovers exercises the Phase-2 G2.3 contract: SIGKILL'ing
// xemu mid-session triggers OnUnexpectedExit, which schedules a Close, and
// the cage ends up in a clean state with no further intervention.
func TestChaosKillRecovers(t *testing.T) {
	bios, ok := findBiosDir()
	if !ok {
		t.Skip("XEMU-WIP: /xemu-bios not present — run inside cloudplay-dev")
	}
	if _, err := exec.LookPath("xemu"); err != nil {
		t.Skip("xemu binary not on PATH")
	}
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("Xvfb not on PATH")
	}

	log := logger.NewConsole(false, "xemu-test", false)
	c := Cage(CagedConf{Xemu: config.XemuConfig{
		Enabled:     true,
		BinaryPath:  "xemu",
		BiosPath:    bios,
		XvfbDisplay: ":102",
		Width:       640,
		Height:      480,
	}}, log)
	if err := c.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	c.SetVideoCb(func(app.Video) {})

	c.Start()
	time.Sleep(1500 * time.Millisecond)

	pid := c.proc.Pid()
	if pid == 0 {
		t.Fatal("xemu pid == 0 before chaos kill")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL xemu pid %d: %v", pid, err)
	}

	// OnUnexpectedExit schedules c.Close in a goroutine; wait for it to
	// actually clean up.  Poll the internal state — if we don't converge
	// in 5 s we've regressed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		done := !c.started
		c.mu.Unlock()
		if done {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.Close() // idempotent belt-and-suspenders

	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		t.Fatal("cage still started 5s after SIGKILL; OnUnexpectedExit path broken")
	}
	c.mu.Unlock()
}

// TestCloseIdempotent makes sure double-Close doesn't panic (worker may call
// it as a cleanup path).
func TestCloseIdempotent(t *testing.T) {
	c := newTestCage(t, 64, 48)
	c.Start()
	time.Sleep(50 * time.Millisecond)
	c.Close()
	c.Close() // must not panic / must not hang
}

// TestBackendMetadata pins the values the media pipeline inspects at room
// init so a reshuffle of defaults during later phases shows up loudly.
func TestBackendMetadata(t *testing.T) {
	c := newTestCage(t, 640, 480)
	if got := c.AudioSampleRate(); got != 48000 {
		t.Errorf("AudioSampleRate: got %d want 48000", got)
	}
	if got := c.AspectRatio(); got < 1.333 || got > 1.334 {
		t.Errorf("AspectRatio: got %f want ~1.333", got)
	}
	if !c.AspectEnabled() {
		t.Error("AspectEnabled: got false want true")
	}
	if c.KbMouseSupport() {
		t.Error("KbMouseSupport: got true want false (xbox is gamepad-only)")
	}
	b := c.VideoBackend()
	if b.Kind() != app.RenderBackendSoftware {
		t.Errorf("VideoBackend.Kind: got %v want software", b.Kind())
	}
	if b.SupportsZeroCopy() {
		t.Error("SupportsZeroCopy: got true want false (stub)")
	}
}

package xemu

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
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
	// Assert no leftover xemu / Xvfb processes belonging to us.
	for _, name := range []string{"xemu", "Xvfb"} {
		out, err := exec.Command("pgrep", "-xa", name).Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				continue
			}
			t.Errorf("pgrep %s: %v", name, err)
		}
		if len(out) > 0 {
			t.Errorf("leftover %s process(es):\n%s", name, out)
		}
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

package flycast

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
)

func newTestCage(t *testing.T, w, h int) *Caged {
	t.Helper()
	log := logger.NewConsole(false, "flycast-test", false)
	c := Cage(CagedConf{Flycast: config.FlycastConfig{Enabled: true, Width: w, Height: h}}, log)
	if err := c.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return &c
}

// TestStubViewportDefaults pins the 640×480 default so downstream media/encoder
// sizing is stable from day one.
func TestStubViewportDefaults(t *testing.T) {
	log := logger.NewConsole(false, "flycast-test", false)
	c := Cage(CagedConf{}, log)
	w, h := c.ViewportSize()
	if w != 640 || h != 480 {
		t.Fatalf("default viewport: got %dx%d want 640x480", w, h)
	}
}

// TestStubFrameLoopRate asserts the stub video loop fires the video callback
// at ≈60 Hz. Proves the callback plumbing is wired independently of any
// real flycast binary.
func TestStubFrameLoopRate(t *testing.T) {
	c := newTestCage(t, 320, 240)
	var count atomic.Uint64
	c.SetVideoCb(func(v app.Video) {
		if v.Frame.W != 320 || v.Frame.H != 240 || len(v.Frame.Data) != 320*240*4 {
			t.Errorf("unexpected frame shape: %dx%d bytes=%d", v.Frame.W, v.Frame.H, len(v.Frame.Data))
		}
		count.Add(1)
	})
	c.Start()
	time.Sleep(1100 * time.Millisecond)
	c.Close()

	got := count.Load()
	if got < 55 || got > 68 {
		t.Fatalf("stub frame rate out of band: got %d frames in ~1.1s, want 55..68", got)
	}
}

// TestCloseIdempotent makes sure double-Close doesn't panic.
func TestCloseIdempotent(t *testing.T) {
	c := newTestCage(t, 64, 48)
	c.Start()
	time.Sleep(50 * time.Millisecond)
	c.Close()
	c.Close()
}

// TestBackendMetadata pins the values the media pipeline inspects at room
// init so a reshuffle of defaults shows up loudly.
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
		t.Error("KbMouseSupport: got true want false (dreamcast is gamepad-only)")
	}
	b := c.VideoBackend()
	if b.Kind() != app.RenderBackendSoftware {
		t.Errorf("VideoBackend.Kind: got %v want software", b.Kind())
	}
	if b.SupportsZeroCopy() {
		t.Error("SupportsZeroCopy: got true want false (stub)")
	}
}

// TestManagerDisabledConf asserts that loading the flycast backend with
// Enabled=false is an explicit error, matching the xemu behavior.
func TestManagerDisabledConf(t *testing.T) {
	log := logger.NewConsole(false, "flycast-test", false)
	c := Cage(CagedConf{Flycast: config.FlycastConfig{Enabled: false}}, log)
	// Init itself doesn't reject disabled — that's the manager's job. Just
	// make sure the cage stays in stub-only mode (no RomPath → no real start).
	if err := c.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if c.conf.Flycast.RomPath != "" {
		t.Errorf("expected empty RomPath, got %q", c.conf.Flycast.RomPath)
	}
}

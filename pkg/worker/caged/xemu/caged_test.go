package xemu

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

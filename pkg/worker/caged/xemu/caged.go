// Package xemu implements the native-emulator backend that drives xemu
// (the original-Xbox emulator) as an external OS process and exposes it
// through the app.App interface the worker's media pipeline already speaks.
//
// Phase 1 (this file): no process management yet. Caged emits a synthetic
// RGBA gradient at 60 Hz so the video callback plumbing can be exercised
// end-to-end before the real xemu subprocess lands in Phase 2.
package xemu

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
)

type CagedConf struct {
	Xemu config.XemuConfig
}

type Caged struct {
	conf CagedConf
	log  *logger.Logger

	videoCb atomic.Pointer[func(app.Video)]
	audioCb atomic.Pointer[func(app.Audio)]
	dataCb  atomic.Pointer[func([]byte)]

	mu      sync.Mutex
	started bool
	stopCh  chan struct{}
	doneCh  chan struct{}

	frameNum uint64
	w, h     int
}

const (
	defaultWidth  = 640
	defaultHeight = 480
	targetFPS     = 60
)

func Cage(conf CagedConf, log *logger.Logger) Caged {
	w, h := conf.Xemu.Width, conf.Xemu.Height
	if w <= 0 {
		w = defaultWidth
	}
	if h <= 0 {
		h = defaultHeight
	}
	return Caged{conf: conf, log: log, w: w, h: h}
}

func (c *Caged) Name() string { return "xemu" }

func (c *Caged) Init() error {
	c.log.Info().Str("binary", c.conf.Xemu.BinaryPath).
		Str("bios", c.conf.Xemu.BiosPath).
		Int("w", c.w).Int("h", c.h).
		Msg("[XEMU-CAGE] registered (stub — Phase 1)")
	return nil
}

// --- app.App surface (stub behavior for Phase 1) -----------------------------

func (c *Caged) AudioSampleRate() int     { return 48000 }
func (c *Caged) AspectRatio() float32     { return 4.0 / 3.0 }
func (c *Caged) AspectEnabled() bool      { return true }
func (c *Caged) ViewportSize() (int, int) { return c.w, c.h }
func (c *Caged) Scale() float64           { return 1.0 }
func (c *Caged) KbMouseSupport() bool     { return false }
func (c *Caged) VideoBackend() app.VideoBackend {
	return stubBackend{}
}

func (c *Caged) SetVideoCb(cb func(app.Video))           { c.videoCb.Store(&cb) }
func (c *Caged) SetAudioCb(cb func(app.Audio))           { c.audioCb.Store(&cb) }
func (c *Caged) SetDataCb(cb func([]byte))               { c.dataCb.Store(&cb) }
func (c *Caged) EmitData(_ []byte)                       {}
func (c *Caged) Input(_ int, _ byte, _ []byte)           {}

func (c *Caged) Start() {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.stopCh = make(chan struct{})
	c.doneCh = make(chan struct{})
	c.mu.Unlock()
	go c.runStubFrameLoop()
}

func (c *Caged) Close() {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return
	}
	stop := c.stopCh
	done := c.doneCh
	c.started = false
	c.mu.Unlock()
	close(stop)
	<-done
	c.log.Info().Uint64("frames", c.frameNum).Msg("[XEMU-CAGE] stopped")
}

// --- stub frame source --------------------------------------------------------

func (c *Caged) runStubFrameLoop() {
	defer close(c.doneCh)
	frameDur := time.Second / targetFPS
	frameDurNs := int32(frameDur)
	buf := make([]byte, c.w*c.h*4)
	tick := time.NewTicker(frameDur)
	defer tick.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-tick.C:
			cbp := c.videoCb.Load()
			if cbp == nil {
				continue
			}
			fillStubFrame(buf, c.w, c.h, c.frameNum)
			c.frameNum++
			(*cbp)(app.Video{
				Frame:    app.RawFrame{Data: buf, Stride: c.w * 4, W: c.w, H: c.h},
				Duration: frameDurNs,
			})
			if c.frameNum%targetFPS == 0 {
				c.log.Debug().Uint64("n", c.frameNum).Msg("[XEMU-CAGE] stub frame")
			}
		}
	}
}

// fillStubFrame writes a deterministic RGBA gradient: red ramps with X,
// green ramps with Y, blue cycles with the frame counter. A 20×20 block
// in the top-left encodes the low byte of the frame number as a solid
// brightness so you can eyeball whether frames are advancing.
func fillStubFrame(buf []byte, w, h int, frameNum uint64) {
	b := byte(frameNum & 0xff)
	for y := 0; y < h; y++ {
		ry := byte(y * 255 / h)
		row := y * w * 4
		for x := 0; x < w; x++ {
			i := row + x*4
			buf[i+0] = byte(x * 255 / w)
			buf[i+1] = ry
			buf[i+2] = b
			buf[i+3] = 0xff
		}
	}
	// Frame-counter patch, top-left 20×20.
	for y := 0; y < 20 && y < h; y++ {
		for x := 0; x < 20 && x < w; x++ {
			i := (y*w + x) * 4
			buf[i+0] = b
			buf[i+1] = b
			buf[i+2] = b
			buf[i+3] = 0xff
		}
	}
}

// --- video backend stub (no hardware render path yet) -------------------------

type stubBackend struct{}

func (stubBackend) Kind() app.RenderBackendKind                { return app.RenderBackendSoftware }
func (stubBackend) Name() string                               { return "xemu-stub" }
func (stubBackend) SupportsZeroCopy() bool                     { return false }
func (stubBackend) ZeroCopyFd(_, _ uint) (int, uint64, error)  { return -1, 0, nil }
func (stubBackend) WaitFrameReady() error                      { return nil }

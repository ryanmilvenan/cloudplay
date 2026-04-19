// Package xemu implements the native-emulator backend that drives xemu
// (the original-Xbox emulator) as an external OS process and exposes it
// through the app.App interface the worker's media pipeline already speaks.
//
// Phase 2 (current): Caged supervises a real Xvfb + xemu pair on Start and
// tears both down on Close. The video callback is still a synthetic gradient
// — frame capture from xemu lands in Phase 3. If Conf.BiosPath is empty,
// only the stub frame loop runs (Phase-1-compatible mode), which keeps
// existing unit tests meaningful.
package xemu

import (
	"fmt"
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

	xvfb *Xvfb
	proc *Process
	vcap *Videocap
	pwse *PipeWireSession
	acap *Audiocap
	pad  *VirtualPad

	// liveFramesRecv is non-zero once the real capture path has delivered
	// at least one frame; the stub loop then pauses its emissions so the
	// downstream pipeline sees exactly one frame stream.
	liveFramesRecv atomic.Bool

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

// LiveFramesActive reports whether the real capture path has produced at
// least one frame since Start. Useful for tests/harnesses that want to
// distinguish stub-emitter frames from xemu-rendered frames without
// needing to sample pixel dimensions.
func (c *Caged) LiveFramesActive() bool { return c.liveFramesRecv.Load() }

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
func (c *Caged) EmitData(_ []byte) {}

func (c *Caged) Input(port int, _ byte, data []byte) {
	if c.pad == nil {
		return
	}
	// TODO Phase 5+: route by port when we add multi-pad support. For now
	// the single pad swallows all ports; revisit when joiner sessions ship.
	if err := c.pad.Inject(data); err != nil {
		c.log.Warn().Err(err).Int("port", port).Msg("[XEMU-INPUT] inject failed")
	}
}

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

	// If BiosPath is empty, stay in stub-only mode (Phase 1 compat — keeps
	// unit tests like TestStubFrameLoopRate meaningful).
	if c.conf.Xemu.BiosPath != "" {
		if err := c.startProcess(); err != nil {
			c.log.Error().Err(err).Msg("[XEMU-CAGE] xemu+xvfb start failed; falling back to stub-only")
			c.teardownProcess()
		}
	}

	go c.runStubFrameLoop()
}

func (c *Caged) startProcess() error {
	display := c.conf.Xemu.XvfbDisplay
	if display == "" {
		display = ":100"
	}
	c.xvfb = &Xvfb{
		Display: display,
		Screen:  fmt.Sprintf("%dx%dx24", c.w, c.h),
		Log:     c.log,
	}
	if err := c.xvfb.Start(); err != nil {
		return fmt.Errorf("xvfb: %w", err)
	}

	// Videocap is an ffmpeg x11grab pipe that reads the Xvfb display xemu
	// paints to. Startup order: Xvfb (above) → xemu → videocap. ffmpeg
	// needs xemu to be actively drawing before its probesize phase
	// completes, otherwise ffmpeg errors out; starting Videocap AFTER xemu
	// avoids that race. liveFramesRecv still gates the stub emitter so
	// downstream only ever sees one stream.
	c.vcap = &Videocap{
		Log:     c.log,
		Display: display,
		Width:   c.w,
		Height:  c.h,
	}

	// Optional Phase-4 audio capture. Launched BEFORE xemu so the
	// pulse socket exists by the time xemu tries to connect. The audio
	// callback discovery inside Audiocap polls until xemu registers a
	// sink-input, so a short startup race is tolerated.
	var pulseSrv, pulseRun string
	if c.conf.Xemu.AudioCapture {
		c.pwse = &PipeWireSession{Log: c.log}
		if err := c.pwse.Start(); err != nil {
			c.log.Error().Err(err).Msg("[XEMU-CAGE] pipewire start failed; audio disabled")
			c.pwse = nil
		} else {
			pulseSrv = c.pwse.PulseServer()
			pulseRun = c.pwse.RuntimeDir()
		}
	}

	c.proc = &Process{
		Conf:            c.conf.Xemu,
		Display:         display,
		PulseServer:     pulseSrv,
		PulseRuntimeDir: pulseRun,
		Log:             c.log,
		OnUnexpectedExit: func(err error) {
			// Don't block the waiter goroutine — hand off to a closer.
			// Close is idempotent and safe to call from anywhere.
			go c.Close()
		},
	}
	if err := c.proc.Start(); err != nil {
		return fmt.Errorf("process: %w", err)
	}

	// Give xemu a beat to open its window and start rendering — ffmpeg
	// x11grab's initial probesize pass needs a live frame to lock on,
	// otherwise it errors out with "cannot open display" races.
	time.Sleep(500 * time.Millisecond)
	if err := c.vcap.Start(c.onRealVideoFrame); err != nil {
		return fmt.Errorf("videocap: %w", err)
	}

	if c.pwse != nil {
		c.acap = &Audiocap{
			Log:             c.log,
			AppName:         "xemu",
			PulseServer:     pulseSrv,
			PulseRuntimeDir: pulseRun,
		}
		if err := c.acap.Start(c.onRealAudioFrame); err != nil {
			c.log.Warn().Err(err).Msg("[XEMU-CAGE] audiocap start failed; continuing without audio")
			c.acap = nil
		}
	}

	if c.conf.Xemu.InputInject {
		c.pad = &VirtualPad{
			Log:        c.log,
			DeviceName: "Microsoft X-Box 360 pad",
			Port:       0,
		}
		if err := c.pad.Open(); err != nil {
			c.log.Warn().Err(err).Msg("[XEMU-CAGE] virtual pad open failed; continuing without input")
			c.pad = nil
		}
	}

	return nil
}

// onRealAudioFrame forwards captured audio chunks to the downstream callback.
func (c *Caged) onRealAudioFrame(au app.Audio) {
	cbp := c.audioCb.Load()
	if cbp == nil {
		return
	}
	(*cbp)(au)
}

// onRealVideoFrame receives frames from the videocap receiver and forwards
// them to the currently-registered video callback. First live frame flips
// liveFramesRecv so the stub loop stops emitting.
func (c *Caged) onRealVideoFrame(v app.Video) {
	if c.liveFramesRecv.CompareAndSwap(false, true) {
		c.log.Info().Int("w", v.Frame.W).Int("h", v.Frame.H).
			Msg("[XEMU-CAGE] first live frame — stub emitter parked")
	}
	cbp := c.videoCb.Load()
	if cbp == nil {
		return
	}
	(*cbp)(v)
}

// teardownProcess closes audiocap, videocap, process, pipewire, virtual pad,
// xvfb if present. Safe to call multiple times and when any component is nil.
// Order matters: kill parec before the pulse server it's connected to; kill
// the xemu process before the videocap shim closes its socket; destroy the
// uinput device after xemu exits so SDL doesn't race on a gone-away device;
// kill xvfb last.
func (c *Caged) teardownProcess() {
	if c.acap != nil {
		_ = c.acap.Close()
		c.acap = nil
	}
	if c.proc != nil {
		_ = c.proc.Close()
		c.proc = nil
	}
	if c.pad != nil {
		_ = c.pad.Close()
		c.pad = nil
	}
	if c.vcap != nil {
		_ = c.vcap.Close()
		c.vcap = nil
	}
	if c.pwse != nil {
		_ = c.pwse.Close()
		c.pwse = nil
	}
	if c.xvfb != nil {
		_ = c.xvfb.Close()
		c.xvfb = nil
	}
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

	// Close the stub frame loop first so downstream consumers don't see
	// frames while the process is being torn down.
	close(stop)
	<-done

	c.teardownProcess()
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
			// Park the stub emitter once the real capture path produces
			// frames — otherwise the encoder would see two interleaved
			// streams and everything downstream breaks.
			if c.liveFramesRecv.Load() {
				continue
			}
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

// Package xemu implements the native-emulator backend that drives xemu
// (the original-Xbox emulator) as an external OS process and exposes it
// through the app.App interface the worker's media pipeline already speaks.
//
// The process/display/capture/input primitives are factored out into the
// sibling nativeemu package so flycast and any future native-process
// backend can compose the same building blocks. This file only holds the
// xemu-specific glue: Xbox viewport defaults, BIOS-gated stub-only mode
// (used by unit tests), and the four-port pad fanout tuned to xemu's SDL
// joystick GUID bindings.
package xemu

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/nativeemu"
)

const (
	logPrefixProc  = "[XEMU-PROC] "
	logPrefixXvfb  = "[XEMU-XVFB] "
	logPrefixVideo = "[XEMU-VIDEO] "
	logPrefixAudio = "[XEMU-AUDIO] "
	logPrefixInput = "[XEMU-INPUT] "
	logPrefixCage  = "[XEMU-CAGE] "
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

	xvfb *nativeemu.Xvfb
	proc *nativeemu.Process
	vcap *nativeemu.Videocap
	pwse *nativeemu.PipeWireSession
	acap *nativeemu.Audiocap
	pads [xboxPorts]*nativeemu.VirtualPad

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
	// xboxPorts is the hardware cap: the Xbox USB expander has exactly four
	// controller ports. We create one uinput-backed virtual pad per port at
	// session start and bind them 1:1 to xemu's [input.bindings] entries so
	// joiners map cleanly onto player 2-4.
	xboxPorts = 4
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
// least one frame since Start.
func (c *Caged) LiveFramesActive() bool { return c.liveFramesRecv.Load() }

// SetDvd configures the ISO path xemu should mount as the DVD drive on
// the next Start. Must be called before Start; has no effect afterward.
func (c *Caged) SetDvd(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conf.Xemu.DvdPath = path
}

func (c *Caged) Init() error {
	c.log.Info().Str("binary", c.conf.Xemu.BinaryPath).
		Str("bios", c.conf.Xemu.BiosPath).
		Int("w", c.w).Int("h", c.h).
		Msgf("%sregistered", logPrefixCage)
	return nil
}

// --- app.App surface ---------------------------------------------------------

func (c *Caged) AudioSampleRate() int     { return 48000 }
func (c *Caged) AspectRatio() float32     { return 4.0 / 3.0 }
func (c *Caged) AspectEnabled() bool      { return true }
func (c *Caged) ViewportSize() (int, int) { return c.w, c.h }
func (c *Caged) Scale() float64           { return 1.0 }
func (c *Caged) KbMouseSupport() bool     { return false }
func (c *Caged) VideoBackend() app.VideoBackend {
	return stubBackend{}
}

func (c *Caged) SetVideoCb(cb func(app.Video)) { c.videoCb.Store(&cb) }
func (c *Caged) SetAudioCb(cb func(app.Audio)) { c.audioCb.Store(&cb) }
func (c *Caged) SetDataCb(cb func([]byte))     { c.dataCb.Store(&cb) }
func (c *Caged) EmitData(_ []byte)             {}

func (c *Caged) Input(port int, _ byte, data []byte) {
	if port < 0 || port >= xboxPorts {
		return
	}
	pad := c.pads[port]
	if pad == nil {
		return
	}
	if err := pad.Inject(data); err != nil {
		c.log.Warn().Err(err).Int("port", port).
			Msgf("%sinject failed", logPrefixInput)
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

	// If BiosPath is empty, stay in stub-only mode (keeps unit tests like
	// TestStubFrameLoopRate meaningful without a BIOS dir mounted).
	if c.conf.Xemu.BiosPath != "" {
		if err := c.startProcess(); err != nil {
			c.log.Error().Err(err).
				Msgf("%sxemu+xvfb start failed; falling back to stub-only", logPrefixCage)
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
	c.xvfb = &nativeemu.Xvfb{
		Display:   display,
		Screen:    fmt.Sprintf("%dx%dx24", c.w, c.h),
		Log:       c.log,
		LogPrefix: logPrefixXvfb,
	}
	if err := c.xvfb.Start(); err != nil {
		return fmt.Errorf("xvfb: %w", err)
	}

	// Videocap reads the Xvfb display xemu paints to. Startup order: Xvfb →
	// xemu → videocap. ffmpeg x11grab needs xemu to be actively drawing
	// before probesize completes; starting Videocap after xemu avoids that
	// race. liveFramesRecv still gates the stub emitter so downstream only
	// ever sees one stream.
	c.vcap = &nativeemu.Videocap{
		Log:       c.log,
		LogPrefix: logPrefixVideo,
		Display:   display,
		Width:     c.w,
		Height:    c.h,
	}

	// Optional audio capture. Launched BEFORE xemu so the pulse socket
	// exists by the time xemu tries to connect. Audiocap's sink-input
	// discovery polls, so a short startup race is tolerated.
	var pulseSrv, pulseRun string
	if c.conf.Xemu.AudioCapture {
		c.pwse = &nativeemu.PipeWireSession{Log: c.log, LogPrefix: logPrefixAudio}
		if err := c.pwse.Start(); err != nil {
			c.log.Error().Err(err).
				Msgf("%spipewire start failed; audio disabled", logPrefixCage)
			c.pwse = nil
		} else {
			pulseSrv = c.pwse.PulseServer()
			pulseRun = c.pwse.RuntimeDir()
		}
	}

	// Virtual pads must exist BEFORE xemu launches so xemu's SDL enumerates
	// them at initialization time. Linux SDL2 hotplug is unreliable without
	// a running udevd (the container has none), so opening them post-launch
	// lands them after xemu's one-shot SDL_Init joystick scan — xemu sees
	// zero controllers and binds nothing to the Xbox USB ports.
	if c.conf.Xemu.InputInject {
		for port := 0; port < xboxPorts; port++ {
			pad := &nativeemu.VirtualPad{
				Log:        c.log,
				LogPrefix:  logPrefixInput,
				DeviceName: "Microsoft X-Box 360 pad",
				Port:       port,
			}
			if err := pad.Open(); err != nil {
				c.log.Warn().Err(err).Int("port", port).
					Msgf("%svirtual pad open failed; skipping port", logPrefixCage)
				continue
			}
			c.pads[port] = pad
		}
	}

	if err := writeXemuConfig(c.conf.Xemu, c.conf.Xemu.DvdPath); err != nil {
		return err
	}

	bin := c.conf.Xemu.BinaryPath
	if bin == "" {
		bin = "xemu"
	}
	c.proc = &nativeemu.Process{
		Bin:       bin,
		Env:       buildXemuEnv(display, pulseSrv, pulseRun),
		Log:       c.log,
		LogPrefix: logPrefixProc,
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
	// x11grab's initial probesize needs a live frame to lock on, otherwise
	// it errors with "cannot open display" races.
	time.Sleep(500 * time.Millisecond)
	if err := c.vcap.Start(c.onRealVideoFrame); err != nil {
		return fmt.Errorf("videocap: %w", err)
	}

	if c.pwse != nil {
		c.acap = &nativeemu.Audiocap{
			Log:             c.log,
			LogPrefix:       logPrefixAudio,
			AppName:         "xemu",
			PulseServer:     pulseSrv,
			PulseRuntimeDir: pulseRun,
		}
		// Audiocap's discovery can block up to 20s polling pactl. Run it
		// in a goroutine so startProcess returns in time for the
		// coordinator's 10s StartGame RPC window; audio joins the live
		// stream whenever xemu registers its pulse sink-input.
		go func(ac *nativeemu.Audiocap) {
			if err := ac.Start(c.onRealAudioFrame); err != nil {
				c.log.Warn().Err(err).
					Msgf("%saudiocap start failed; continuing without audio", logPrefixCage)
			}
		}(c.acap)
	}

	return nil
}

func (c *Caged) onRealAudioFrame(au app.Audio) {
	cbp := c.audioCb.Load()
	if cbp == nil {
		return
	}
	(*cbp)(au)
}

func (c *Caged) onRealVideoFrame(v app.Video) {
	if c.liveFramesRecv.CompareAndSwap(false, true) {
		c.log.Info().Int("w", v.Frame.W).Int("h", v.Frame.H).
			Msgf("%sfirst live frame — stub emitter parked", logPrefixCage)
	}
	cbp := c.videoCb.Load()
	if cbp == nil {
		return
	}
	(*cbp)(v)
}

// teardownProcess closes audiocap, videocap, process, pipewire, virtual pad,
// xvfb if present. Safe to call multiple times and when any component is nil.
// Order matters: kill parec before the pulse server; kill xemu before the
// videocap shim closes its socket; destroy the uinput device after xemu
// exits; kill xvfb last.
func (c *Caged) teardownProcess() {
	if c.acap != nil {
		_ = c.acap.Close()
		c.acap = nil
	}
	if c.proc != nil {
		_ = c.proc.Close()
		c.proc = nil
	}
	for i, pad := range c.pads {
		if pad != nil {
			_ = pad.Close()
			c.pads[i] = nil
		}
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
	c.log.Info().Uint64("frames", c.frameNum).
		Msgf("%sstopped", logPrefixCage)
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
				c.log.Debug().Uint64("n", c.frameNum).
					Msgf("%sstub frame", logPrefixCage)
			}
		}
	}
}

// fillStubFrame writes a deterministic RGBA gradient: red ramps with X,
// green ramps with Y, blue cycles with the frame counter. A 20×20 block in
// the top-left encodes the low byte of the frame number as a solid
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

func (stubBackend) Kind() app.RenderBackendKind               { return app.RenderBackendSoftware }
func (stubBackend) Name() string                              { return "xemu-stub" }
func (stubBackend) SupportsZeroCopy() bool                    { return false }
func (stubBackend) ZeroCopyFd(_, _ uint) (int, uint64, error) { return -1, 0, nil }
func (stubBackend) WaitFrameReady() error                     { return nil }

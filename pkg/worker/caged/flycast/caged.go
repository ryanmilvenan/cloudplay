// Package flycast implements the native-emulator backend that drives flycast
// (the Sega Dreamcast emulator) as an external OS process and exposes it
// through the app.App interface the worker's media pipeline already speaks.
//
// This sibling of pkg/worker/caged/xemu composes the same pkg/worker/caged/
// nativeemu primitives (Xvfb, Videocap, PipeWireSession, Audiocap,
// VirtualPad, Process) with Dreamcast-specific glue: four-port pad fanout,
// emu.cfg templating, and a stub frame loop for BIOS-less test runs.
//
// Microphone support (the whole reason this adapter exists — Seaman) is
// wired in a later phase via nativeemu.VirtualMicSource + emu.cfg.
package flycast

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/nativeemu"
)

const (
	logPrefixProc  = "[FLYCAST-PROC] "
	logPrefixXvfb  = "[FLYCAST-XVFB] "
	logPrefixVideo = "[FLYCAST-VIDEO] "
	logPrefixAudio = "[FLYCAST-AUDIO] "
	logPrefixInput = "[FLYCAST-INPUT] "
	logPrefixMic   = "[FLYCAST-MIC] "
	logPrefixCage  = "[FLYCAST-CAGE] "
)

type CagedConf struct {
	Flycast config.FlycastConfig
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
	vmic *nativeemu.VirtualMicSource
	pads [dreamcastPorts]*nativeemu.VirtualPad

	// liveFramesRecv is non-zero once the real capture path has delivered
	// at least one frame; the stub loop then pauses so the downstream
	// pipeline sees exactly one frame stream.
	liveFramesRecv atomic.Bool

	frameNum uint64
	w, h     int
}

const (
	defaultWidth  = 640
	defaultHeight = 480
	targetFPS     = 60
	// dreamcastPorts is the hardware cap — the DC exposes four maple bus
	// ports. Most games use only port A; Seaman is single-player.
	dreamcastPorts = 4
)

func Cage(conf CagedConf, log *logger.Logger) Caged {
	w, h := conf.Flycast.Width, conf.Flycast.Height
	if w <= 0 {
		w = defaultWidth
	}
	if h <= 0 {
		h = defaultHeight
	}
	return Caged{conf: conf, log: log, w: w, h: h}
}

func (c *Caged) Name() string { return "flycast" }

// LiveFramesActive reports whether the real capture path has produced at
// least one frame since Start.
func (c *Caged) LiveFramesActive() bool { return c.liveFramesRecv.Load() }

// SetRom configures the ROM flycast should load on the next Start. Must be
// called before Start; has no effect afterward.
func (c *Caged) SetRom(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conf.Flycast.RomPath = path
}

func (c *Caged) Init() error {
	c.log.Info().Str("binary", c.conf.Flycast.BinaryPath).
		Str("bios", c.conf.Flycast.BiosPath).
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

func (c *Caged) Input(port int, device byte, data []byte) {
	if device == app.InputMicrophone {
		if c.vmic == nil {
			return
		}
		if _, err := c.vmic.Write(data); err != nil {
			c.log.Warn().Err(err).Msgf("%swrite failed", logPrefixMic)
		}
		return
	}
	if port < 0 || port >= dreamcastPorts {
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

	// Stub-only mode when RomPath is unset — keeps unit tests meaningful
	// without a ROM or flycast binary present.
	if c.conf.Flycast.RomPath != "" {
		if err := c.startProcess(); err != nil {
			c.log.Error().Err(err).
				Msgf("%sflycast+xvfb start failed; falling back to stub-only", logPrefixCage)
			c.teardownProcess()
		}
	}

	go c.runStubFrameLoop()
}

func (c *Caged) startProcess() error {
	display := c.conf.Flycast.XvfbDisplay
	if display == "" {
		display = ":110"
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

	c.vcap = &nativeemu.Videocap{
		Log:       c.log,
		LogPrefix: logPrefixVideo,
		Display:   display,
		Width:     c.w,
		Height:    c.h,
	}

	var pulseSrv, pulseRun string
	if c.conf.Flycast.AudioCapture {
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

	// Virtual pads must exist BEFORE flycast launches so SDL enumerates
	// them at initialization time. Without a running udevd in the
	// container, SDL2 hotplug is unreliable; opening pads post-launch
	// lands them after flycast's one-shot joystick scan.
	if c.conf.Flycast.InputInject {
		for port := 0; port < dreamcastPorts; port++ {
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

	// Mic uplink: load a PulseAudio pipe-source BEFORE flycast starts so
	// the source exists when SDL enumerates capture devices. Flycast opens
	// it by name via PULSE_SOURCE (see buildFlycastEnv). Requires the
	// PipeWire session to be up — mic silently no-ops when AudioCapture
	// is disabled.
	micSourceName := ""
	if c.conf.Flycast.Mic && c.pwse != nil {
		name := c.conf.Flycast.MicSourceName
		if name == "" {
			name = "cloudplay-mic"
		}
		rate := c.conf.Flycast.MicRate
		if rate <= 0 {
			rate = 11025
		}
		c.vmic = &nativeemu.VirtualMicSource{
			Log:             c.log,
			LogPrefix:       logPrefixMic,
			PulseServer:     pulseSrv,
			PulseRuntimeDir: pulseRun,
			SourceName:      name,
			Rate:            rate,
			Channels:        1,
		}
		if err := c.vmic.Start(); err != nil {
			c.log.Error().Err(err).Msgf("%svirtual mic start failed; continuing without mic", logPrefixCage)
			c.vmic = nil
		} else {
			micSourceName = name
		}
	}

	if err := writeFlycastConfig(c.conf.Flycast); err != nil {
		return err
	}

	// Flycast writes VMU saves to $XDG_DATA_HOME/flycast (defaulting to
	// $HOME/.local/share/flycast). The dir doesn't exist in a fresh
	// container, so flycast logs "Failed to create VMU save file" on boot
	// and games like Seaman that need persisted VMU state regress. Make
	// the dir exist before launch; durable-across-restarts saves remain
	// out-of-scope (would need a quadlet bind mount like libretro/saves).
	if home, err := os.UserHomeDir(); err == nil {
		_ = os.MkdirAll(filepath.Join(home, ".local", "share", "flycast"), 0o755)
	}

	bin := c.conf.Flycast.BinaryPath
	if bin == "" {
		bin = "flycast"
	}
	c.proc = &nativeemu.Process{
		Bin:       bin,
		Args:      []string{c.conf.Flycast.RomPath},
		Env:       buildFlycastEnv(display, pulseSrv, pulseRun, c.conf.Flycast.ConfigDir, micSourceName),
		Log:       c.log,
		LogPrefix: logPrefixProc,
		OnUnexpectedExit: func(err error) {
			go c.Close()
		},
	}
	if err := c.proc.Start(); err != nil {
		return fmt.Errorf("process: %w", err)
	}

	// Give flycast a beat to open its window and start rendering — ffmpeg
	// x11grab's probesize needs a live frame to lock on.
	time.Sleep(500 * time.Millisecond)
	if err := c.vcap.Start(c.onRealVideoFrame); err != nil {
		return fmt.Errorf("videocap: %w", err)
	}

	if c.pwse != nil {
		c.acap = &nativeemu.Audiocap{
			Log:             c.log,
			LogPrefix:       logPrefixAudio,
			// Flycast registers with pulse as lowercase "flycast" via SDL's
			// default application-name hint, not "Flycast" — pactl list
			// sink-inputs confirmed this empirically on v2.6.
			AppName:         "flycast",
			PulseServer:     pulseSrv,
			PulseRuntimeDir: pulseRun,
		}
		if err := c.acap.Start(c.onRealAudioFrame); err != nil {
			c.log.Warn().Err(err).
				Msgf("%saudiocap start failed; continuing without audio", logPrefixCage)
			c.acap = nil
		}
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

// teardownProcess closes everything in reverse-dependency order: audiocap
// before the pulse server it's connected to; flycast before videocap's
// pipe closes; uinput devices after flycast exits; the virtual mic source
// before the pulse server hosting its module; xvfb last.
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
	if c.vmic != nil {
		_ = c.vmic.Close()
		c.vmic = nil
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
		}
	}
}

// fillStubFrame writes a deterministic RGBA gradient: red ramps with X, green
// ramps with Y, blue cycles with the frame counter. The top-left 20×20 block
// encodes the low byte of the frame number as a monochrome brightness so it
// is visually obvious whether frames are advancing.
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

// --- video backend stub -------------------------------------------------------

type stubBackend struct{}

func (stubBackend) Kind() app.RenderBackendKind               { return app.RenderBackendSoftware }
func (stubBackend) Name() string                              { return "flycast-stub" }
func (stubBackend) SupportsZeroCopy() bool                    { return false }
func (stubBackend) ZeroCopyFd(_, _ uint) (int, uint64, error) { return -1, 0, nil }
func (stubBackend) WaitFrameReady() error                     { return nil }

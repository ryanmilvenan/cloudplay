package nativeemu

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
)

// Videocap captures an Xvfb display through an ffmpeg x11grab pipe and routes
// raw RGBA frames into the configured app.Video callback.
//
// Why x11grab rather than an LD_PRELOAD GL hook: many SDL2 emulators create
// offscreen GL contexts in addition to their visible window, and a naive
// GL-swap hook can latch onto the wrong surface. x11grab reads the final
// composited pixels — whatever the emulator puts on screen. If the
// GPU→CPU readback cost ever becomes the bottleneck, callers can swap in an
// LD_PRELOAD variant behind the same Videocap surface.
type Videocap struct {
	// Log receives lifecycle and ffmpeg stderr.
	Log *logger.Logger
	// LogPrefix tags every log line. Defaults to "[NATIVE-VIDEO] " when empty.
	LogPrefix string
	// Display is the X display ffmpeg captures from (e.g. ":100").
	Display string
	// Width/Height is the Xvfb screen geometry we capture.
	Width, Height int
	// Framerate paces ffmpeg's captures. Defaults to 60.
	Framerate int

	cb func(app.Video)

	mu         sync.Mutex
	started    bool
	closing    atomic.Bool
	doneCh     chan struct{}
	cmd        *exec.Cmd
	stdout     io.ReadCloser
	framesRecv atomic.Uint64
}

func (v *Videocap) logPrefix() string {
	if v.LogPrefix == "" {
		return "[NATIVE-VIDEO] "
	}
	return v.LogPrefix
}

// Start spawns ffmpeg x11grab and a reader goroutine. Returns once the
// subprocess is up; actual frame delivery begins asynchronously.
func (v *Videocap) Start(cb func(app.Video)) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.started {
		return errors.New("videocap: already started")
	}
	if cb == nil {
		return errors.New("videocap: callback is required")
	}
	if v.Display == "" {
		return errors.New("videocap: Display is required")
	}
	if v.Width <= 0 || v.Height <= 0 {
		return errors.New("videocap: Width/Height are required")
	}
	if v.Framerate <= 0 {
		v.Framerate = 60
	}
	v.cb = cb

	v.cmd = exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "warning",
		"-thread_queue_size", "1024",
		"-probesize", "32",
		"-f", "x11grab",
		"-framerate", strconv.Itoa(v.Framerate),
		"-video_size", fmt.Sprintf("%dx%d", v.Width, v.Height),
		"-i", v.Display,
		"-f", "rawvideo", "-pix_fmt", "rgba",
		"-",
	)
	v.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	v.cmd.Stderr = newStreamLogger(v.Log, v.logPrefix()+"ffmpeg ")
	stdout, err := v.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("videocap: stdout pipe: %w", err)
	}
	v.stdout = stdout
	if err := v.cmd.Start(); err != nil {
		return fmt.Errorf("videocap: ffmpeg start: %w", err)
	}
	v.started = true
	v.doneCh = make(chan struct{})
	go v.readLoop()
	v.Log.Info().Int("pid", v.cmd.Process.Pid).Str("display", v.Display).
		Int("w", v.Width).Int("h", v.Height).Int("fps", v.Framerate).
		Msgf("%sffmpeg x11grab spawned", v.logPrefix())
	return nil
}

// Close sends SIGTERM to ffmpeg and waits for the reader loop to drain.
// Safe to call multiple times; safe before Start.
func (v *Videocap) Close() error {
	v.mu.Lock()
	if !v.started {
		v.mu.Unlock()
		return nil
	}
	if !v.closing.CompareAndSwap(false, true) {
		v.mu.Unlock()
		return nil
	}
	done := v.doneCh
	cmd := v.cmd
	v.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if cmd != nil && cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-done
	}
	v.Log.Info().Uint64("frames", v.framesRecv.Load()).
		Msgf("%scapture closed", v.logPrefix())
	return nil
}

// FramesReceived reports cumulative frames delivered to the callback.
func (v *Videocap) FramesReceived() uint64 { return v.framesRecv.Load() }

func (v *Videocap) readLoop() {
	defer close(v.doneCh)
	stride := v.Width * 4
	frameSize := stride * v.Height
	buf := make([]byte, frameSize)
	frameDurNs := int32(time.Second / time.Duration(v.Framerate))
	lastLog := time.Now()
	since := uint64(0)

	for {
		if v.closing.Load() {
			return
		}
		if _, err := io.ReadFull(v.stdout, buf); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrClosed) {
				return
			}
			if !v.closing.Load() {
				v.Log.Warn().Err(err).Msgf("%sffmpeg read failed", v.logPrefix())
			}
			return
		}
		// Fresh slice — downstream consumers may hold references past the
		// next ffmpeg write.
		frame := make([]byte, frameSize)
		copy(frame, buf)

		v.framesRecv.Add(1)
		since++
		v.cb(app.Video{
			Frame:    app.RawFrame{Data: frame, Stride: stride, W: v.Width, H: v.Height},
			Duration: frameDurNs,
		})

		if time.Since(lastLog) >= 5*time.Second {
			v.Log.Info().
				Uint64("frames_5s", since).
				Uint64("total", v.framesRecv.Load()).
				Msgf("%sflow", v.logPrefix())
			lastLog = time.Now()
			since = 0
		}
	}
}

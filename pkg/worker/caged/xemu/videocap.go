package xemu

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

// Videocap captures the xemu process's Xvfb display through an ffmpeg
// x11grab pipe and routes raw RGBA frames into the configured app.Video
// callback.
//
// Why x11grab rather than the LD_PRELOAD GL hook we originally shipped:
// xemu creates four SDL windows (three offscreen GL-backed contexts for
// internal render passes plus one visible output window). A naive
// SDL_GL_SwapWindow hook grabs whichever window swaps first — and it is
// always an offscreen one rendering xemu's idle overlay. x11grab reads
// the final composited pixels xemu puts on screen, full stop. See
// docs/capture-path-not-taken-ld-preload.md for the re-entry notes if
// the x11grab GPU→CPU cost ever becomes the bottleneck.
type Videocap struct {
	// Log receives lifecycle and ffmpeg stderr.
	Log *logger.Logger
	// Display is the X display ffmpeg captures from (e.g. ":100").
	Display string
	// Width/Height is the Xvfb screen geometry we capture — the xemu
	// window fills this when we keep Xvfb the same size as xemu's
	// viewport.
	Width, Height int
	// Framerate paces ffmpeg's captures. 60 matches xemu's render loop.
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

	// x11grab → rawvideo pipe. -probesize 32 and -thread_queue_size 1024
	// keep startup quick and absorb short bursts without dropping frames.
	// -pix_fmt rgba matches app.RawFrame so we don't need a conversion
	// step; ffmpeg does the X11 BGRA-to-RGBA swap on the fly.
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
	v.cmd.Stderr = newStreamLogger(v.Log, "[XEMU-VIDEO:ffmpeg] ")
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
		Msg("[XEMU-VIDEO] ffmpeg x11grab spawned")
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
	v.Log.Info().Uint64("frames", v.framesRecv.Load()).Msg("[XEMU-VIDEO] capture closed")
	return nil
}

// FramesReceived reports cumulative frames delivered to the callback.
func (v *Videocap) FramesReceived() uint64 { return v.framesRecv.Load() }

// SocketPath returns empty — this field is retained for code-path symmetry
// with the libretro backend / earlier capture implementation, callers that
// plumbed a Unix socket path no longer need to.
func (v *Videocap) SocketPath() string { return "" }

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
				v.Log.Warn().Err(err).Msg("[XEMU-VIDEO] ffmpeg read failed")
			}
			return
		}
		// Copy into a fresh slice so downstream consumers (encoder,
		// recorder) can hold references without racing with the next
		// ffmpeg write into our shared buffer.
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
				Msg("[XEMU-VIDEO] flow")
			lastLog = time.Now()
			since = 0
		}
	}
}

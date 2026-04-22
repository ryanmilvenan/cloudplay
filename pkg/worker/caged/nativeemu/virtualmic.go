package nativeemu

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// VirtualMicSource exposes an in-process PCM stream as a named PulseAudio
// source the emulator can open as its microphone input. Built on
// PulseAudio's module-pipe-source:
//
//  1. Start: `pactl load-module module-pipe-source` (targeting the cage's
//     private pipewire-pulse instance) creates a FIFO-backed source.
//  2. Write: PCM bytes written to the FIFO flow out of the source in real
//     time; the consumer (SDL2 inside flycast) sees a live microphone.
//  3. Close: the FIFO is closed, the module is unloaded, the FIFO file is
//     removed.
//
// The default format is S16LE mono at 11025 Hz — the Dreamcast microphone's
// native sample rate. Change via the Rate/Channels fields before Start.
// The browser-side AudioWorklet resamples from the AudioContext rate
// (typically 48 kHz) before pushing chunks over the WebRTC data channel
// so no resampling is needed on this side.
type VirtualMicSource struct {
	// Log receives lifecycle + pactl diagnostics. Required.
	Log *logger.Logger
	// LogPrefix tags every log line. Defaults to "[NATIVE-MIC] " when empty.
	LogPrefix string
	// PulseServer is the PULSE_SERVER URI pactl uses to load the module.
	// Required; typically sourced from PipeWireSession.PulseServer().
	PulseServer string
	// PulseRuntimeDir mirrors XDG_RUNTIME_DIR for the pactl client lookup.
	// Required; typically sourced from PipeWireSession.RuntimeDir().
	PulseRuntimeDir string
	// SourceName is the Pulse source name (e.g. "cloudplay-mic"). The
	// emulator opens this by name via PULSE_SOURCE=<SourceName>. Defaults
	// to "cloudplay-mic".
	SourceName string
	// FifoPath is the FIFO file the module-pipe-source reads from.
	// Defaults to "<PulseRuntimeDir>/<SourceName>.fifo".
	FifoPath string
	// Rate is the source sample rate. Defaults to 11025 (Dreamcast mic rate).
	Rate int
	// Channels is the source channel count. Defaults to 1 (mono).
	Channels int

	moduleID int

	mu      sync.Mutex
	started bool
	closing atomic.Bool
	fifoFd  int
}

func (v *VirtualMicSource) logPrefix() string {
	if v.LogPrefix == "" {
		return "[NATIVE-MIC] "
	}
	return v.LogPrefix
}

// Start loads the PulseAudio pipe-source module and opens the FIFO for
// writing. On failure, any partially-created state is torn down.
func (v *VirtualMicSource) Start() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.started {
		return errors.New("virtualmic: already started")
	}
	if v.PulseServer == "" || v.PulseRuntimeDir == "" {
		return errors.New("virtualmic: PulseServer and PulseRuntimeDir are required")
	}
	if v.SourceName == "" {
		v.SourceName = "cloudplay-mic"
	}
	if v.FifoPath == "" {
		v.FifoPath = filepath.Join(v.PulseRuntimeDir, v.SourceName+".fifo")
	}
	if v.Rate <= 0 {
		v.Rate = 11025
	}
	if v.Channels <= 0 {
		v.Channels = 1
	}

	// Remove a stale FIFO so mkfifo doesn't EEXIST from a prior crashed run.
	_ = os.Remove(v.FifoPath)
	if err := syscall.Mkfifo(v.FifoPath, 0o600); err != nil {
		return fmt.Errorf("virtualmic: mkfifo %s: %w", v.FifoPath, err)
	}

	arg := fmt.Sprintf(
		"source_name=%s file=%s format=s16le rate=%d channels=%d source_properties=device.description=cloudplay-mic",
		v.SourceName, v.FifoPath, v.Rate, v.Channels,
	)
	cmd := exec.Command("pactl", "load-module", "module-pipe-source", arg)
	cmd.Env = append(os.Environ(),
		"PULSE_SERVER="+v.PulseServer,
		"XDG_RUNTIME_DIR="+v.PulseRuntimeDir,
	)
	out, err := cmd.Output()
	if err != nil {
		_ = os.Remove(v.FifoPath)
		return fmt.Errorf("virtualmic: pactl load-module: %w", err)
	}
	idStr := trimTrailingWhitespace(string(out))
	id, err := strconv.Atoi(idStr)
	if err != nil {
		_ = os.Remove(v.FifoPath)
		return fmt.Errorf("virtualmic: parse module id %q: %w", idStr, err)
	}
	v.moduleID = id

	// Open the FIFO non-blocking so if the emulator never opens its mic
	// input we don't deadlock on the first Write; missing reader produces
	// EPIPE on write which the caller can swallow.
	fd, err := syscall.Open(v.FifoPath, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		// The reader side isn't open yet — PipeWire-Pulse opens it when
		// the source is first consumed. Retry briefly; failing that, keep
		// the module loaded and let Write retry the open on first PCM.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			fd, err = syscall.Open(v.FifoPath, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			v.Log.Warn().Err(err).Str("fifo", v.FifoPath).
				Msgf("%sFIFO not opened yet; will retry on first Write", v.logPrefix())
			fd = -1
		}
	}
	v.fifoFd = fd
	v.started = true
	v.Log.Info().Str("source", v.SourceName).Int("module", v.moduleID).
		Str("fifo", v.FifoPath).Int("rate", v.Rate).Int("ch", v.Channels).
		Msgf("%spipe-source loaded", v.logPrefix())
	return nil
}

// Write sends PCM bytes to the FIFO. Non-blocking: if the emulator hasn't
// opened the source yet (or has stalled) the write returns EAGAIN and the
// chunk is dropped. Caller-side buffering is unnecessary for a live mic
// stream; dropping is preferable to unbounded latency buildup.
func (v *VirtualMicSource) Write(pcm []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.started || v.closing.Load() {
		return 0, errors.New("virtualmic: not started")
	}
	if v.fifoFd < 0 {
		// Lazy open: the emulator-side reader probably just came online.
		fd, err := syscall.Open(v.FifoPath, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return 0, nil // Reader still not ready — drop silently.
		}
		v.fifoFd = fd
	}
	n, err := syscall.Write(v.fifoFd, pcm)
	if err != nil {
		// EAGAIN = reader is slow → drop. EPIPE = reader vanished → close fd,
		// lazy-reopen on next Write.
		if err == syscall.EAGAIN {
			return 0, nil
		}
		if err == syscall.EPIPE {
			_ = syscall.Close(v.fifoFd)
			v.fifoFd = -1
			return 0, nil
		}
		return n, err
	}
	return n, nil
}

// Close unloads the module and removes the FIFO. Safe to call multiple times.
func (v *VirtualMicSource) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.started {
		return nil
	}
	if !v.closing.CompareAndSwap(false, true) {
		return nil
	}
	v.started = false
	if v.fifoFd >= 0 {
		_ = syscall.Close(v.fifoFd)
		v.fifoFd = -1
	}
	if v.moduleID > 0 {
		cmd := exec.Command("pactl", "unload-module", strconv.Itoa(v.moduleID))
		cmd.Env = append(os.Environ(),
			"PULSE_SERVER="+v.PulseServer,
			"XDG_RUNTIME_DIR="+v.PulseRuntimeDir,
		)
		if err := cmd.Run(); err != nil {
			v.Log.Warn().Err(err).Int("module", v.moduleID).
				Msgf("%spactl unload-module failed", v.logPrefix())
		}
	}
	if v.FifoPath != "" {
		_ = os.Remove(v.FifoPath)
	}
	v.Log.Info().Str("source", v.SourceName).
		Msgf("%spipe-source closed", v.logPrefix())
	return nil
}

func trimTrailingWhitespace(s string) string {
	for len(s) > 0 {
		r := s[len(s)-1]
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

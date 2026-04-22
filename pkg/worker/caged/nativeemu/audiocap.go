package nativeemu

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
)

// Audiocap captures the stereo S16LE output of a PulseAudio/PipeWire client
// and fires the configured callback once per ~ChunkMs milliseconds.
//
// Lifecycle:
//
//	a := Audiocap{Log: log, AppName: "xemu", PulseServer: "unix:...", PulseRuntimeDir: "..."}
//	a.Start(onAudio)
//	...
//	a.Close()
//
// The capture discovers the target stream by polling `pactl list sink-inputs`
// for an `application.name` match, then spawns `parec --monitor-stream=<idx>`
// whose stdout is the raw S16LE 48 kHz stereo byte stream.
type Audiocap struct {
	// Log receives diagnostics; required.
	Log *logger.Logger
	// LogPrefix tags every log line. Defaults to "[NATIVE-AUDIO] " when empty.
	LogPrefix string
	// AppName is the PulseAudio application.name of the stream to capture.
	AppName string
	// PulseServer is the PULSE_SERVER URI; required.
	PulseServer string
	// PulseRuntimeDir is the XDG_RUNTIME_DIR the server lives under; required.
	PulseRuntimeDir string
	// DiscoveryTimeout gives up if the sink-input doesn't appear. Default 20 s.
	DiscoveryTimeout time.Duration
	// ChunkMs is the callback cadence. Default 10 ms → ~960 samples.
	ChunkMs int

	cmd        *exec.Cmd
	stdoutPipe io.ReadCloser
	cb         func(app.Audio)

	mu         sync.Mutex
	started    bool
	closing    atomic.Bool
	doneCh     chan struct{}
	bytesRecv  atomic.Uint64
	lastRMS    atomic.Uint64
	chunksRecv atomic.Uint64
}

func (a *Audiocap) logPrefix() string {
	if a.LogPrefix == "" {
		return "[NATIVE-AUDIO] "
	}
	return a.LogPrefix
}

// Start polls for the target stream, spawns parec, and begins forwarding
// chunks to cb. Returns once parec is running (or the discovery window
// expires with no match).
func (a *Audiocap) Start(cb func(app.Audio)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started {
		return errors.New("audiocap: already started")
	}
	if cb == nil {
		return errors.New("audiocap: callback is required")
	}
	if a.AppName == "" {
		return errors.New("audiocap: AppName is required")
	}
	if a.PulseServer == "" || a.PulseRuntimeDir == "" {
		return errors.New("audiocap: PulseServer and PulseRuntimeDir are required")
	}
	if a.DiscoveryTimeout == 0 {
		a.DiscoveryTimeout = 20 * time.Second
	}
	if a.ChunkMs <= 0 {
		a.ChunkMs = 10
	}
	a.cb = cb

	ctx, cancel := context.WithTimeout(context.Background(), a.DiscoveryTimeout)
	idx, err := a.discoverSinkInput(ctx)
	cancel()
	if err != nil {
		return err
	}
	a.Log.Info().Str("app", a.AppName).Int("idx", idx).
		Msgf("%starget stream located", a.logPrefix())

	a.cmd = exec.Command("parec",
		"--monitor-stream="+strconv.Itoa(idx),
		"--format=s16le",
		"--rate=48000",
		"--channels=2",
		"--latency-msec="+strconv.Itoa(a.ChunkMs),
	)
	a.cmd.Env = append(os.Environ(),
		"PULSE_SERVER="+a.PulseServer,
		"XDG_RUNTIME_DIR="+a.PulseRuntimeDir,
	)
	stdout, err := a.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("audiocap: stdout pipe: %w", err)
	}
	a.cmd.Stderr = newStreamLogger(a.Log, a.logPrefix()+"parec ")
	a.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	a.stdoutPipe = stdout

	if err := a.cmd.Start(); err != nil {
		return fmt.Errorf("audiocap: start parec: %w", err)
	}
	a.started = true
	a.doneCh = make(chan struct{})
	go a.readLoop()
	a.Log.Info().Int("pid", a.cmd.Process.Pid).
		Msgf("%sparec spawned", a.logPrefix())
	return nil
}

// Close SIGTERMs parec and waits for the read loop to drain.
func (a *Audiocap) Close() error {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return nil
	}
	if !a.closing.CompareAndSwap(false, true) {
		a.mu.Unlock()
		return nil
	}
	done := a.doneCh
	cmd := a.cmd
	a.mu.Unlock()

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
	a.Log.Info().
		Uint64("chunks", a.chunksRecv.Load()).
		Uint64("bytes", a.bytesRecv.Load()).
		Msgf("%scapture closed", a.logPrefix())
	return nil
}

// BytesReceived reports the cumulative capture volume. Useful for tests.
func (a *Audiocap) BytesReceived() uint64 { return a.bytesRecv.Load() }

// ChunksReceived reports the cumulative callback count.
func (a *Audiocap) ChunksReceived() uint64 { return a.chunksRecv.Load() }

// LastRMS returns the most recent per-chunk RMS value.
func (a *Audiocap) LastRMS() float64 {
	return math.Float64frombits(a.lastRMS.Load())
}

func (a *Audiocap) readLoop() {
	defer close(a.doneCh)

	samplesPerChunk := 48000 * 2 * a.ChunkMs / 1000
	bytesPerChunk := samplesPerChunk * 2
	buf := make([]byte, bytesPerChunk)
	r := bufio.NewReaderSize(a.stdoutPipe, bytesPerChunk*4)
	frameDurNs := int32(time.Duration(a.ChunkMs) * time.Millisecond)

	lastLog := time.Now()
	sinceLog := uint64(0)
	for {
		if a.closing.Load() {
			return
		}
		n, err := io.ReadFull(r, buf)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return
			}
			if !a.closing.Load() {
				a.Log.Warn().Err(err).Msgf("%sparec read failed", a.logPrefix())
			}
			return
		}
		a.bytesRecv.Add(uint64(n))
		a.chunksRecv.Add(1)
		sinceLog++

		samples := make([]int16, samplesPerChunk)
		for i := 0; i < samplesPerChunk; i++ {
			samples[i] = int16(binary.LittleEndian.Uint16(buf[i*2 : i*2+2]))
		}
		updateRMS(&a.lastRMS, samples)

		a.cb(app.Audio{Data: samples, Duration: frameDurNs})

		if time.Since(lastLog) >= 5*time.Second {
			a.Log.Info().
				Uint64("chunks_5s", sinceLog).
				Float64("last_rms", math.Float64frombits(a.lastRMS.Load())).
				Uint64("total_bytes", a.bytesRecv.Load()).
				Msgf("%sflow", a.logPrefix())
			lastLog = time.Now()
			sinceLog = 0
		}
	}
}

func updateRMS(slot *atomic.Uint64, samples []int16) {
	if len(samples) == 0 {
		return
	}
	var sumSq float64
	for _, s := range samples {
		f := float64(s)
		sumSq += f * f
	}
	rms := math.Sqrt(sumSq / float64(len(samples)))
	slot.Store(math.Float64bits(rms))
}

func (a *Audiocap) discoverSinkInput(ctx context.Context) (int, error) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		idx, ok := a.listAndFind()
		if ok {
			return idx, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("audiocap: %q sink-input not found within %s", a.AppName, a.DiscoveryTimeout)
		case <-tick.C:
		}
	}
}

func (a *Audiocap) listAndFind() (int, bool) {
	cmd := exec.Command("pactl", "list", "sink-inputs")
	cmd.Env = append(os.Environ(),
		"PULSE_SERVER="+a.PulseServer,
		"XDG_RUNTIME_DIR="+a.PulseRuntimeDir,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	var (
		curID   int
		haveID  bool
		matched bool
	)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Sink Input #") {
			if haveID && matched {
				return curID, true
			}
			idStr := strings.TrimPrefix(line, "Sink Input #")
			n, err := strconv.Atoi(idStr)
			if err != nil {
				haveID = false
				continue
			}
			curID = n
			haveID = true
			matched = false
			continue
		}
		if haveID && strings.HasPrefix(line, "application.name = ") {
			v := strings.TrimPrefix(line, "application.name = ")
			v = strings.Trim(v, `"`)
			if v == a.AppName {
				matched = true
			}
		}
	}
	if haveID && matched {
		return curID, true
	}
	return 0, false
}

// audio-canary: validates Phase-4 PipeWire audio capture end-to-end by
// emitting a known tone, capturing it through the same pkg/worker/caged/xemu
// Audiocap path the cage uses, and FFTing the result.
//
// Phase-4 validation gate G4.1: FFT peak at 440 Hz (±2 Hz) for 5 s continuous.
//
// Usage (inside the dev container):
//
//	scripts/dev-sync.sh harness audio-canary
//	scripts/dev-sync.sh harness audio-canary -freq 440 -duration 5s -tol 2
//
// Does NOT touch xemu; the target stream is paplay itself.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"math/cmplx"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/xemu"
)

func main() {
	var (
		freq     = flag.Float64("freq", 440, "sine frequency, Hz")
		duration = flag.Duration("duration", 5*time.Second, "how long to capture")
		tol      = flag.Float64("tol", 2, "peak-bin tolerance in Hz")
		minPeak  = flag.Float64("min-peak-db", -6, "minimum peak magnitude vs. integrated noise floor (dB)")
	)
	flag.Parse()

	log := logger.NewConsole(false, "audio-canary", false)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	sess := &xemu.PipeWireSession{Log: log}
	if err := sess.Start(); err != nil {
		die(err)
	}
	defer sess.Close()

	// Emit the sine via paplay from a raw-pcm fifo fed by our goroutine.
	// paplay's application.name ends up as "paplay".
	toneR, toneW := io.Pipe()
	defer toneR.Close()
	defer toneW.Close()

	const rate = 48000
	const ch = 2
	toneCtx, toneCancel := context.WithCancel(ctx)
	defer toneCancel()
	go emitSine(toneCtx, toneW, *freq, rate, ch)

	play := exec.Command("paplay",
		"--rate="+fmt.Sprintf("%d", rate),
		"--channels="+fmt.Sprintf("%d", ch),
		"--format=s16le",
		"--raw",
	)
	play.Env = append(os.Environ(),
		"PULSE_SERVER="+sess.PulseServer(),
		"XDG_RUNTIME_DIR="+sess.RuntimeDir(),
	)
	play.Stdin = toneR
	play.Stdout = nil
	play.Stderr = os.Stderr
	play.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := play.Start(); err != nil {
		die(fmt.Errorf("paplay start: %w", err))
	}
	defer func() {
		if play.Process != nil {
			_ = syscall.Kill(-play.Process.Pid, syscall.SIGTERM)
			_ = play.Wait()
		}
	}()
	log.Info().Int("pid", play.Process.Pid).Msg("paplay emitting sine")

	cap := &xemu.Audiocap{
		Log:              log,
		AppName:          "paplay",
		PulseServer:      sess.PulseServer(),
		PulseRuntimeDir:  sess.RuntimeDir(),
		DiscoveryTimeout: 5 * time.Second,
	}

	// Collect captured samples into a single big slice for FFT.
	var (
		mu      sync.Mutex
		samples []int16
		gotAny  = make(chan struct{}, 1)
	)
	if err := cap.Start(func(a app.Audio) {
		mu.Lock()
		samples = append(samples, a.Data...)
		mu.Unlock()
		select {
		case gotAny <- struct{}{}:
		default:
		}
	}); err != nil {
		die(fmt.Errorf("audiocap start: %w", err))
	}
	defer cap.Close()

	// Wait for first chunk, then capture for 'duration'.
	select {
	case <-gotAny:
	case <-time.After(3 * time.Second):
		die(fmt.Errorf("no audio arrived within 3s"))
	case <-ctx.Done():
		die(ctx.Err())
	}
	log.Info().Msg("first chunk received; collecting...")
	time.Sleep(*duration)

	mu.Lock()
	raw := append([]int16(nil), samples...)
	mu.Unlock()

	if len(raw) < rate*ch {
		die(fmt.Errorf("not enough samples: got %d want >= %d", len(raw), rate*ch))
	}

	// Reduce to mono, window, FFT. Use only the middle portion to avoid
	// start/end transients.
	mono := toMono(raw, ch)
	skip := len(mono) / 4
	segment := mono[skip : len(mono)-skip]
	// Round to nearest power of two ≤ len.
	n := 1 << 16 // 65536 samples = ~1.37s @ 48kHz — good frequency resolution
	if len(segment) < n {
		n = 1
		for n*2 <= len(segment) {
			n *= 2
		}
	}
	segment = segment[:n]
	hann(segment)
	spectrum := fft(segment)

	binHz := float64(rate) / float64(n)
	peakBin := 0
	peakMag := 0.0
	// Only consider the first half (Nyquist).
	for i := 1; i < n/2; i++ {
		m := cmplx.Abs(spectrum[i])
		if m > peakMag {
			peakMag = m
			peakBin = i
		}
	}
	peakHz := float64(peakBin) * binHz

	// Integrated noise floor = median magnitude of all non-peak bins.
	noise := medianMag(spectrum, peakBin)
	peakDb := 20 * math.Log10(peakMag/noise)

	log.Info().
		Float64("want_hz", *freq).
		Float64("peak_hz", peakHz).
		Float64("bin_hz", binHz).
		Float64("peak_vs_noise_db", peakDb).
		Int("n", n).
		Msg("FFT result")

	fail := false
	if math.Abs(peakHz-*freq) > *tol {
		fmt.Fprintf(os.Stderr, "FAIL: peak at %.2f Hz, want %.2f ± %.2f\n", peakHz, *freq, *tol)
		fail = true
	}
	if peakDb < *minPeak {
		fmt.Fprintf(os.Stderr, "FAIL: peak only %.1f dB above noise, want >= %.1f\n", peakDb, *minPeak)
		fail = true
	}
	if fail {
		os.Exit(1)
	}
	fmt.Printf("PASS: peak %.2f Hz (want %.2f±%.2f), %.1f dB above noise\n", peakHz, *freq, *tol, peakDb)
}

// emitSine writes an endless 48 kHz S16LE stereo sine to w.
func emitSine(ctx context.Context, w io.WriteCloser, hz float64, rate, ch int) {
	defer w.Close()
	const block = 4096
	buf := make([]byte, block*ch*2)
	phase := 0.0
	delta := 2 * math.Pi * hz / float64(rate)
	for {
		if ctx.Err() != nil {
			return
		}
		for i := 0; i < block; i++ {
			s := int16(math.Sin(phase) * 0.5 * 32767)
			phase += delta
			if phase > 2*math.Pi {
				phase -= 2 * math.Pi
			}
			for c := 0; c < ch; c++ {
				off := (i*ch + c) * 2
				binary.LittleEndian.PutUint16(buf[off:off+2], uint16(s))
			}
		}
		if _, err := w.Write(buf); err != nil {
			return
		}
	}
}

func toMono(s []int16, ch int) []float64 {
	out := make([]float64, len(s)/ch)
	for i := range out {
		sum := 0
		for c := 0; c < ch; c++ {
			sum += int(s[i*ch+c])
		}
		out[i] = float64(sum) / float64(ch)
	}
	return out
}

func hann(x []float64) {
	n := len(x)
	for i := range x {
		w := 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(n-1)))
		x[i] *= w
	}
}

// fft performs an in-place iterative Cooley-Tukey radix-2 DFT. len(x) must
// be a power of two.
func fft(x []float64) []complex128 {
	n := len(x)
	out := make([]complex128, n)
	for i, v := range x {
		out[i] = complex(v, 0)
	}
	// Bit-reversal permutation.
	j := 0
	for i := 1; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			out[i], out[j] = out[j], out[i]
		}
	}
	// Butterflies.
	for size := 2; size <= n; size <<= 1 {
		half := size >> 1
		table := make([]complex128, half)
		for k := 0; k < half; k++ {
			angle := -2 * math.Pi * float64(k) / float64(size)
			table[k] = complex(math.Cos(angle), math.Sin(angle))
		}
		for start := 0; start < n; start += size {
			for k := 0; k < half; k++ {
				t := table[k] * out[start+k+half]
				u := out[start+k]
				out[start+k] = u + t
				out[start+k+half] = u - t
			}
		}
	}
	return out
}

func medianMag(s []complex128, skipBin int) float64 {
	n := len(s) / 2
	mags := make([]float64, 0, n-1)
	for i := 1; i < n; i++ {
		if i == skipBin {
			continue
		}
		mags = append(mags, cmplx.Abs(s[i]))
	}
	// Quick partial sort to median.
	for i := 0; i <= len(mags)/2; i++ {
		minIdx := i
		for j := i + 1; j < len(mags); j++ {
			if mags[j] < mags[minIdx] {
				minIdx = j
			}
		}
		mags[i], mags[minIdx] = mags[minIdx], mags[i]
	}
	if len(mags) == 0 {
		return 1
	}
	m := mags[len(mags)/2]
	if m <= 0 {
		return 1
	}
	return m
}

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}

// uinput-canary: validates Phase-5's VirtualPad by driving a scripted
// sequence of RetroPad-format input packets through the real
// pkg/worker/caged/xemu.VirtualPad and asserting a separate evtest process
// sees the expected evdev events.
//
// Phase-5 G5.1: golden diff is empty across all 14 Xbox controls.
//
// Usage (inside the dev container):
//
//	scripts/dev-sync.sh harness uinput-canary
//	scripts/dev-sync.sh harness uinput-canary -write-goldens
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/xemu"
)

const (
	// libretro RetroPad bit indices (mirrors pkg/worker/caged/xemu/input.go
	// constants; duplicated here so this tool stays standalone).
	bitB      = 0
	bitY      = 1
	bitSelect = 2
	bitStart  = 3
	bitUp     = 4
	bitDown   = 5
	bitLeft   = 6
	bitRight  = 7
	bitA      = 8
	bitX      = 9
	bitL      = 10
	bitR      = 11
	bitL3     = 14
	bitR3     = 15
)

type step struct {
	name string
	btns uint16
	lx   int16
	ly   int16
	rx   int16
	ry   int16
	lt   int16
	rt   int16
	hold time.Duration
}

func main() {
	var (
		goldenPath   = flag.String("golden", "tools/uinput-canary/testdata/golden_events.txt", "path to golden event log")
		writeGoldens = flag.Bool("write-goldens", false, "overwrite goldens with the captured events instead of diffing")
		dumpCaptured = flag.String("dump-captured", "", "if set, write the captured (normalized) event log here for inspection")
	)
	flag.Parse()

	log := logger.NewConsole(false, "uinput-canary", false)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pad := &xemu.VirtualPad{Log: log, DeviceName: "cloudplay-canary-pad"}
	if err := pad.Open(); err != nil {
		die(fmt.Errorf("pad open: %w", err))
	}
	defer pad.Close()

	// Wait briefly for udev to populate /dev/input/eventN then resolve path.
	var devPath string
	for i := 0; i < 20; i++ {
		devPath = pad.DevicePath()
		if devPath != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if devPath == "" {
		die(fmt.Errorf("could not find event node for our virtual pad"))
	}
	log.Info().Str("path", devPath).Msg("found our event node")

	// Spawn evtest, grab its stdout in a goroutine.
	evtCtx, evtCancel := context.WithCancel(ctx)
	defer evtCancel()
	evt := exec.CommandContext(evtCtx, "evtest", devPath)
	evt.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	evtOut, err := evt.StdoutPipe()
	if err != nil {
		die(fmt.Errorf("evtest stdout: %w", err))
	}
	evt.Stderr = os.Stderr
	if err := evt.Start(); err != nil {
		die(fmt.Errorf("evtest start: %w", err))
	}
	defer func() {
		if evt.Process != nil {
			_ = syscall.Kill(-evt.Process.Pid, syscall.SIGTERM)
			_ = evt.Wait()
		}
	}()

	linesCh := make(chan string, 1024)
	go func() {
		defer close(linesCh)
		sc := bufio.NewScanner(evtOut)
		for sc.Scan() {
			linesCh <- sc.Text()
		}
	}()

	// Let evtest finish its device-description preamble before we start
	// injecting — otherwise its early "Properties:" lines interleave with
	// our events and the golden becomes order-sensitive.
	time.Sleep(400 * time.Millisecond)

	// Scripted sequence: press and release each button individually, then
	// exercise the sticks and triggers, and finally a D-pad sweep.
	seq := []step{
		{name: "A down", btns: 1 << bitB, hold: 50 * time.Millisecond},
		{name: "A up", hold: 50 * time.Millisecond},
		{name: "B down", btns: 1 << bitA, hold: 50 * time.Millisecond},
		{name: "B up", hold: 50 * time.Millisecond},
		{name: "X down", btns: 1 << bitY, hold: 50 * time.Millisecond},
		{name: "X up", hold: 50 * time.Millisecond},
		{name: "Y down", btns: 1 << bitX, hold: 50 * time.Millisecond},
		{name: "Y up", hold: 50 * time.Millisecond},
		{name: "LB down", btns: 1 << bitL, hold: 50 * time.Millisecond},
		{name: "LB up", hold: 50 * time.Millisecond},
		{name: "RB down", btns: 1 << bitR, hold: 50 * time.Millisecond},
		{name: "RB up", hold: 50 * time.Millisecond},
		{name: "Back down", btns: 1 << bitSelect, hold: 50 * time.Millisecond},
		{name: "Back up", hold: 50 * time.Millisecond},
		{name: "Start down", btns: 1 << bitStart, hold: 50 * time.Millisecond},
		{name: "Start up", hold: 50 * time.Millisecond},
		{name: "LS click", btns: 1 << bitL3, hold: 50 * time.Millisecond},
		{name: "LS release", hold: 50 * time.Millisecond},
		{name: "RS click", btns: 1 << bitR3, hold: 50 * time.Millisecond},
		{name: "RS release", hold: 50 * time.Millisecond},
		{name: "LS +X", lx: 16384, hold: 50 * time.Millisecond},
		{name: "LS center", hold: 50 * time.Millisecond},
		{name: "RS +Y", ry: 16384, hold: 50 * time.Millisecond},
		{name: "RS center", hold: 50 * time.Millisecond},
		{name: "LT half", lt: 16384, hold: 50 * time.Millisecond},
		{name: "LT off", hold: 50 * time.Millisecond},
		{name: "RT full", rt: 32767, hold: 50 * time.Millisecond},
		{name: "RT off", hold: 50 * time.Millisecond},
		{name: "DPad up", btns: 1 << bitUp, hold: 50 * time.Millisecond},
		{name: "DPad right", btns: 1 << bitRight, hold: 50 * time.Millisecond},
		{name: "DPad down", btns: 1 << bitDown, hold: 50 * time.Millisecond},
		{name: "DPad left", btns: 1 << bitLeft, hold: 50 * time.Millisecond},
		{name: "DPad center", hold: 50 * time.Millisecond},
	}
	for _, s := range seq {
		pkt := packRetroPad(s)
		if err := pad.Inject(pkt); err != nil {
			die(fmt.Errorf("inject %q: %w", s.name, err))
		}
		time.Sleep(s.hold)
	}
	// Settle — wait for evtest to flush all events.
	time.Sleep(500 * time.Millisecond)
	evtCancel()
	// Drain up to 500 ms of remaining lines.
	collectDeadline := time.Now().Add(500 * time.Millisecond)
	var captured []string
	for {
		select {
		case line, ok := <-linesCh:
			if !ok {
				goto done
			}
			captured = append(captured, line)
			collectDeadline = time.Now().Add(200 * time.Millisecond)
		case <-time.After(time.Until(collectDeadline) + 50*time.Millisecond):
			goto done
		}
	}
done:

	normalized := normalizeEvents(captured)
	out := strings.Join(normalized, "\n") + "\n"

	if *dumpCaptured != "" {
		if err := os.WriteFile(*dumpCaptured, []byte(out), 0o644); err != nil {
			die(fmt.Errorf("dump: %w", err))
		}
	}

	if *writeGoldens {
		if err := os.MkdirAll(goldenDir(*goldenPath), 0o755); err != nil {
			die(err)
		}
		if err := os.WriteFile(*goldenPath, []byte(out), 0o644); err != nil {
			die(fmt.Errorf("write golden: %w", err))
		}
		fmt.Printf("wrote %d normalized events to %s\n", len(normalized), *goldenPath)
		return
	}

	golden, err := os.ReadFile(*goldenPath)
	if err != nil {
		die(fmt.Errorf("read golden %s: %w (run with -write-goldens first?)", *goldenPath, err))
	}
	if bytes.Equal(bytes.TrimSpace(golden), bytes.TrimSpace([]byte(out))) {
		fmt.Printf("PASS: %d events match golden\n", len(normalized))
		return
	}
	fmt.Fprintf(os.Stderr, "FAIL: golden diff (want %d bytes, got %d).\n", len(golden), len(out))
	fmt.Fprintln(os.Stderr, "--- got ---")
	fmt.Fprintln(os.Stderr, out)
	fmt.Fprintln(os.Stderr, "--- want ---")
	fmt.Fprintln(os.Stderr, string(golden))
	os.Exit(1)
}

// packRetroPad serializes a step into the 14-byte libretro wire format.
func packRetroPad(s step) []byte {
	b := make([]byte, 14)
	binary.LittleEndian.PutUint16(b[0:2], s.btns)
	binary.LittleEndian.PutUint16(b[2:4], uint16(s.lx))
	binary.LittleEndian.PutUint16(b[4:6], uint16(s.ly))
	binary.LittleEndian.PutUint16(b[6:8], uint16(s.rx))
	binary.LittleEndian.PutUint16(b[8:10], uint16(s.ry))
	binary.LittleEndian.PutUint16(b[10:12], uint16(s.lt))
	binary.LittleEndian.PutUint16(b[12:14], uint16(s.rt))
	return b
}

// evtest prints lines like:
//
//	Event: time 1744999999.123456, type 1 (EV_KEY), code 304 (BTN_SOUTH), value 1
//
// We strip the volatile timestamp and keep just the stable (type, code,
// value) tuple, collapsing the device-description preamble out of the way.
var eventRe = regexp.MustCompile(`^Event: time [\d.]+,\s+(.*)$`)

func normalizeEvents(lines []string) []string {
	var out []string
	started := false
	for _, l := range lines {
		m := eventRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		started = true
		// Filter SYN_REPORT events — they're framing boilerplate, not signal,
		// and their exact placement depends on write batching.
		if strings.Contains(m[1], "SYN_REPORT") {
			continue
		}
		// Drop "---------------" separator lines evtest interleaves.
		if strings.Contains(m[1], "----") {
			continue
		}
		out = append(out, strings.TrimSpace(m[1]))
	}
	if !started {
		return nil
	}
	// Preserve order — this is a deterministic stream.
	// But the golden-diff must be stable across runs, so don't sort.
	_ = sort.StringsAreSorted // hint: keep sort package in imports (future de-dup)
	return out
}

func goldenDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}

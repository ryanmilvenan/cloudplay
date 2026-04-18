// xemu-smoke: process-lifecycle smoke test for the xemu native backend.
//
// Runs N iterations of {start Xvfb + xemu → wait → stop} and asserts each
// cycle leaves no lingering xemu/Xvfb processes. This is the Phase-2 G2.1
// gate in /Users/rock/.claude/plans/tender-sprouting-diffie.md.
//
// Usage inside the dev container (via dev-sync):
//
//	scripts/dev-sync.sh harness xemu-smoke
//	scripts/dev-sync.sh harness xemu-smoke -n 20 -hold 3s
//
// Requires xemu BIOS files bind-mounted into /xemu-bios/ per the
// cloudplay-dev quadlet.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/xemu"
)

func main() {
	var (
		iters   = flag.Int("n", 10, "number of start/stop iterations")
		hold    = flag.Duration("hold", 5*time.Second, "time to keep xemu alive each iteration")
		display = flag.String("display", ":100", "Xvfb display")
		biosDir = flag.String("bios", "/xemu-bios", "BIOS root dir (expects bios/, mcpx/, hdd/ subdirs)")
		binary  = flag.String("xemu", "xemu", "xemu binary path")
		verbose = flag.Bool("v", false, "log per-iteration progress")
		chaos   = flag.Bool("chaos", false, "kill -9 xemu mid-session; asserts the cage recovers cleanly (G2.3)")
		chaosAt = flag.Duration("chaos-at", 2*time.Second, "when during the hold window to kill -9 xemu (only with -chaos)")
	)
	flag.Parse()

	log := logger.NewConsole(false, "xemu-smoke", false)

	// Sanity: ensure no stale processes before we start.
	if err := assertClean(); err != nil {
		fmt.Fprintf(os.Stderr, "pre-flight: %v\n", err)
		os.Exit(1)
	}

	// Allow ^C to interrupt a long run.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	conf := config.XemuConfig{
		Enabled:     true,
		BinaryPath:  *binary,
		BiosPath:    *biosDir,
		XvfbDisplay: *display,
		Width:       640,
		Height:      480,
	}

	failures := 0
	for i := 1; i <= *iters; i++ {
		if ctx.Err() != nil {
			break
		}
		t0 := time.Now()
		if err := oneIteration(ctx, log, conf, *hold, *verbose, *chaos, *chaosAt); err != nil {
			fmt.Printf("iter %d/%d FAIL in %s: %v\n", i, *iters, time.Since(t0).Round(time.Millisecond), err)
			failures++
			continue
		}
		fmt.Printf("iter %d/%d PASS in %s\n", i, *iters, time.Since(t0).Round(time.Millisecond))
	}

	if failures > 0 {
		fmt.Fprintf(os.Stderr, "\n%d/%d iterations FAILED\n", failures, *iters)
		os.Exit(1)
	}
	fmt.Printf("\nall %d iterations passed\n", *iters)
}

func oneIteration(ctx context.Context, log *logger.Logger, conf config.XemuConfig, hold time.Duration, verbose, chaos bool, chaosAt time.Duration) error {
	cage := xemu.Cage(xemu.CagedConf{Xemu: conf}, log)
	if err := cage.Init(); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	frameCount := 0
	cage.SetVideoCb(func(v app.Video) { frameCount++ })
	cage.SetAudioCb(func(a app.Audio) {})
	cage.SetDataCb(func(b []byte) {})

	cage.Start()
	if chaos {
		// Wait for xemu to actually be running, then SIGKILL it.
		// The cage's OnUnexpectedExit should trigger its own Close()
		// in the background; our Close() below must still return cleanly.
		go func() {
			select {
			case <-ctx.Done():
			case <-time.After(chaosAt):
				if err := exec.Command("pkill", "-9", "-x", "xemu").Run(); err != nil {
					log.Warn().Err(err).Msg("chaos pkill failed (xemu may have exited already)")
				} else {
					log.Info().Msg("CHAOS: SIGKILLed xemu")
				}
			}
		}()
	}
	select {
	case <-ctx.Done():
		cage.Close()
		return ctx.Err()
	case <-time.After(hold):
	}
	cage.Close()

	if verbose {
		log.Info().Int("frames", frameCount).Msg("iteration drained stub frames")
	}
	if err := assertClean(); err != nil {
		return fmt.Errorf("leftover processes: %w", err)
	}
	return nil
}

// assertClean returns nil when no xemu or Xvfb processes are running, else
// an error describing what's still alive. Uses pgrep -x for exact-name
// matches so "xemu-smoke" doesn't collide with "xemu".
func assertClean() error {
	var leftovers []string
	for _, name := range []string{"xemu", "Xvfb"} {
		out, err := exec.Command("pgrep", "-xa", name).Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				continue // no match = clean
			}
			return fmt.Errorf("pgrep %s: %w", name, err)
		}
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			leftovers = append(leftovers, fmt.Sprintf("%s → %s", name, trimmed))
		}
	}
	if len(leftovers) > 0 {
		return fmt.Errorf(strings.Join(leftovers, "; "))
	}
	return nil
}

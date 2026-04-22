package nativeemu

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// Xvfb supervises a virtual X server so the emulator has a display to render
// into without requiring a real GPU head. On moon Xvfb proxies GLX to NVIDIA
// via libglvnd so GL_RENDERER still reports the dGPU.
type Xvfb struct {
	// Display is the X display identifier (e.g. ":100"). Required.
	Display string
	// Screen is the screen geometry ("WIDTHxHEIGHTxDEPTH"), e.g. "640x480x24".
	Screen string
	// Log receives lifecycle messages. Required.
	Log *logger.Logger
	// LogPrefix tags every log line with a caller-chosen scope. Defaults to
	// "[NATIVE-XVFB] " when empty so logs are still distinguishable.
	LogPrefix string

	cmd *exec.Cmd
}

func (x *Xvfb) logPrefix() string {
	if x.LogPrefix == "" {
		return "[NATIVE-XVFB] "
	}
	return x.LogPrefix
}

// Start boots Xvfb and blocks until the display answers xdpyinfo probes.
func (x *Xvfb) Start() error {
	if x.Display == "" {
		return fmt.Errorf("xvfb: Display is required")
	}
	if x.Screen == "" {
		x.Screen = "640x480x24"
	}

	x.cmd = exec.Command("Xvfb",
		x.Display,
		"-screen", "0", x.Screen,
		"-nolisten", "tcp",
		"+extension", "GLX",
		"+extension", "RANDR",
		"+extension", "RENDER",
	)
	if err := x.cmd.Start(); err != nil {
		return fmt.Errorf("xvfb: %w", err)
	}
	x.Log.Info().Int("pid", x.cmd.Process.Pid).Str("display", x.Display).
		Str("screen", x.Screen).Msgf("%sstarted", x.logPrefix())

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("xdpyinfo", "-display", x.Display).Run() == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = x.cmd.Process.Kill()
	_, _ = x.cmd.Process.Wait()
	return fmt.Errorf("xvfb: %s never became ready", x.Display)
}

// Close sends SIGTERM and waits up to 2 s before SIGKILL.
func (x *Xvfb) Close() error {
	if x.cmd == nil || x.cmd.Process == nil {
		return nil
	}
	_ = x.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- x.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = x.cmd.Process.Kill()
		<-done
	}
	x.Log.Info().Str("display", x.Display).Msgf("%sstopped", x.logPrefix())
	x.cmd = nil
	return nil
}

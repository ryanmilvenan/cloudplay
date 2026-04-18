package xemu

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// PipeWireSession supervises a private pipewire + wireplumber + pipewire-pulse
// triplet for a single cage's audio needs. It does *not* touch the system
// pipewire instance (if one exists) — everything lives under a per-session
// XDG_RUNTIME_DIR.
//
// Consumers hook up via PulseServer() → "unix:<runtime>/pulse/native" and
// can pass the same runtime to subprocesses via RuntimeDir().
type PipeWireSession struct {
	// Log receives lifecycle + child stderr. Required.
	Log *logger.Logger
	// RootDir is the parent directory the per-session XDG_RUNTIME_DIR lives
	// under. Defaults to /tmp.
	RootDir string

	runtime string
	pw      *exec.Cmd
	wp      *exec.Cmd
	pulse   *exec.Cmd
}

// Start launches pipewire, wireplumber, and pipewire-pulse in sequence and
// blocks until the pulse socket is accepting connections. Returns an error
// (and tears down any started processes) on failure.
func (p *PipeWireSession) Start() error {
	root := p.RootDir
	if root == "" {
		root = "/tmp"
	}
	p.runtime = filepath.Join(root, fmt.Sprintf("pw-run-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.MkdirAll(p.runtime, 0o700); err != nil {
		return fmt.Errorf("pipewire: mkdir runtime: %w", err)
	}
	env := append(os.Environ(), "XDG_RUNTIME_DIR="+p.runtime)

	var startErr error
	spawn := func(name string, extraEnv ...string) (*exec.Cmd, error) {
		cmd := exec.Command(name)
		cmd.Env = append(env, extraEnv...)
		cmd.Stdout = newStreamLogger(p.Log, "[XEMU-AUDIO:"+name+"] ")
		cmd.Stderr = newStreamLogger(p.Log, "[XEMU-AUDIO:"+name+"] ")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("pipewire: start %s: %w", name, err)
		}
		return cmd, nil
	}

	var err error
	if p.pw, err = spawn("pipewire"); err != nil {
		startErr = err
		goto fail
	}
	time.Sleep(300 * time.Millisecond)
	if p.wp, err = spawn("wireplumber"); err != nil {
		startErr = err
		goto fail
	}
	time.Sleep(300 * time.Millisecond)
	if p.pulse, err = spawn("pipewire-pulse"); err != nil {
		startErr = err
		goto fail
	}

	// Wait for the pulse socket to be ready.
	if err := p.waitSocketReady(5 * time.Second); err != nil {
		startErr = err
		goto fail
	}
	p.Log.Info().Str("runtime", p.runtime).Msg("[XEMU-AUDIO] pipewire session ready")
	return nil

fail:
	p.Close()
	return startErr
}

// Close SIGTERMs all three processes in reverse order.
func (p *PipeWireSession) Close() error {
	for _, c := range []*exec.Cmd{p.pulse, p.wp, p.pw} {
		if c == nil || c.Process == nil {
			continue
		}
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
	}
	done := make(chan struct{})
	go func() {
		for _, c := range []*exec.Cmd{p.pulse, p.wp, p.pw} {
			if c != nil {
				_ = c.Wait()
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		for _, c := range []*exec.Cmd{p.pulse, p.wp, p.pw} {
			if c != nil && c.Process != nil {
				_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
			}
		}
		<-done
	}
	if p.runtime != "" {
		_ = os.RemoveAll(p.runtime)
	}
	p.Log.Info().Msg("[XEMU-AUDIO] pipewire session closed")
	return nil
}

// PulseServer returns the PULSE_SERVER URI clients should use to talk to
// this session's pulse socket. Empty before Start.
func (p *PipeWireSession) PulseServer() string {
	if p.runtime == "" {
		return ""
	}
	return "unix:" + filepath.Join(p.runtime, "pulse", "native")
}

// RuntimeDir returns this session's XDG_RUNTIME_DIR. Empty before Start.
func (p *PipeWireSession) RuntimeDir() string { return p.runtime }

// waitSocketReady polls for the pulse native socket to appear.
func (p *PipeWireSession) waitSocketReady(d time.Duration) error {
	sockPath := filepath.Join(p.runtime, "pulse", "native")
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			// Also try a pactl info to ensure the server is live.
			cmd := exec.Command("pactl", "info")
			cmd.Env = append(os.Environ(),
				"PULSE_SERVER=unix:"+sockPath,
				"XDG_RUNTIME_DIR="+p.runtime,
			)
			if err := cmd.Run(); err == nil {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("pipewire: pulse socket %s never came up in %s", sockPath, d)
}

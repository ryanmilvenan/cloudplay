package nativeemu

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
	// LogPrefix tags every log line. Defaults to "[NATIVE-AUDIO] " when empty.
	LogPrefix string
	// RootDir is the parent directory the per-session XDG_RUNTIME_DIR lives
	// under. Defaults to /tmp.
	RootDir string

	runtime string
	pw      *exec.Cmd
	wp      *exec.Cmd
	pulse   *exec.Cmd
}

func (p *PipeWireSession) logPrefix() string {
	if p.LogPrefix == "" {
		return "[NATIVE-AUDIO] "
	}
	return p.LogPrefix
}

// Start launches pipewire, wireplumber, and pipewire-pulse and blocks until
// the pulse socket is accepting connections.
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
		cmd.Stdout = newStreamLogger(p.Log, p.logPrefix()+name+" ")
		cmd.Stderr = newStreamLogger(p.Log, p.logPrefix()+name+" ")
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

	if err := p.waitSocketReady(5 * time.Second); err != nil {
		startErr = err
		goto fail
	}
	p.Log.Info().Str("runtime", p.runtime).Msgf("%spipewire session ready", p.logPrefix())
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
	p.Log.Info().Msgf("%spipewire session closed", p.logPrefix())
	return nil
}

// PulseServer returns the PULSE_SERVER URI clients should use. Empty before Start.
func (p *PipeWireSession) PulseServer() string {
	if p.runtime == "" {
		return ""
	}
	return "unix:" + filepath.Join(p.runtime, "pulse", "native")
}

// RuntimeDir returns this session's XDG_RUNTIME_DIR. Empty before Start.
func (p *PipeWireSession) RuntimeDir() string { return p.runtime }

func (p *PipeWireSession) waitSocketReady(d time.Duration) error {
	sockPath := filepath.Join(p.runtime, "pulse", "native")
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
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

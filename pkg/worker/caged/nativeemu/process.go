package nativeemu

import (
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// Process supervises a single emulator invocation. It owns the OS process
// and the waiter goroutine that catches unexpected exits. Callers construct
// it per-session and call Start once, Close once.
//
// Lifecycle:
//
//	p := &Process{Bin: "flycast", Args: []string{rom}, Env: env, Log: log, OnUnexpectedExit: fn}
//	p.Start()    // spawns the process, returns once pid is alive
//	... running ...
//	p.Close()    // SIGTERM with 2s grace, then SIGKILL
//
// On an unexpected exit (the emulator crashes or is SIGKILLed externally),
// OnUnexpectedExit fires *once* after the wait returns. Close() suppresses
// the callback because the exit is requested in that path.
type Process struct {
	// Bin is the executable path or name (looked up via $PATH).
	Bin string
	// Args is the argument list (without Bin).
	Args []string
	// Env is the full environment the process runs with. Callers should
	// typically start from os.Environ() and append DISPLAY, audio-routing
	// vars, etc.
	Env []string
	// Log receives lifecycle + stdout/stderr. Required.
	Log *logger.Logger
	// LogPrefix tags every log line. Defaults to "[NATIVE-PROC] " when empty.
	LogPrefix string
	// OnUnexpectedExit is invoked from the waiter goroutine when the process
	// dies without a preceding Close(). Optional.
	OnUnexpectedExit func(err error)

	cmd      *exec.Cmd
	started  atomic.Bool
	closing  atomic.Bool
	waitCh   chan struct{}
	onceExit sync.Once
}

func (p *Process) logPrefix() string {
	if p.LogPrefix == "" {
		return "[NATIVE-PROC] "
	}
	return p.LogPrefix
}

// Start spawns the configured binary and returns once the process is alive.
// It does not wait for any particular state beyond "exec succeeded".
func (p *Process) Start() error {
	if p.started.Load() {
		return fmt.Errorf("process: already started")
	}
	if p.Bin == "" {
		return fmt.Errorf("process: Bin is required")
	}
	if p.Log == nil {
		return fmt.Errorf("process: Log is required")
	}

	p.cmd = exec.Command(p.Bin, p.Args...)
	if p.Env != nil {
		p.cmd.Env = p.Env
	}
	p.cmd.Stdout = newStreamLogger(p.Log, p.logPrefix())
	p.cmd.Stderr = newStreamLogger(p.Log, p.logPrefix())
	// New process group so we can signal the binary + any child threads cleanly.
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := p.cmd.Start(); err != nil {
		return fmt.Errorf("process: start: %w", err)
	}
	p.started.Store(true)
	p.waitCh = make(chan struct{})
	p.Log.Info().Int("pid", p.cmd.Process.Pid).Str("bin", p.Bin).
		Msgf("%sstarted", p.logPrefix())

	go p.waitLoop()
	return nil
}

func (p *Process) waitLoop() {
	err := p.cmd.Wait()
	p.started.Store(false)
	if p.closing.Load() {
		p.Log.Info().Msgf("%sstopped (requested)", p.logPrefix())
	} else {
		p.Log.Error().Err(err).Msgf("%sunexpected exit", p.logPrefix())
		p.onceExit.Do(func() {
			if p.OnUnexpectedExit != nil {
				p.OnUnexpectedExit(err)
			}
		})
	}
	close(p.waitCh)
}

// Close stops the process. Safe to call multiple times and from the
// OnUnexpectedExit callback (guarded by atomic flags). Blocks until the
// process is reaped.
func (p *Process) Close() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if !p.closing.CompareAndSwap(false, true) {
		<-p.waitCh
		return nil
	}
	if !p.started.Load() {
		if p.waitCh != nil {
			<-p.waitCh
		}
		return nil
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.waitCh:
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.waitCh
	}
	return nil
}

// Pid reports the OS pid of the running process, or 0 when not running.
func (p *Process) Pid() int {
	if p.cmd == nil || p.cmd.Process == nil || !p.started.Load() {
		return 0
	}
	return p.cmd.Process.Pid
}

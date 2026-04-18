package xemu

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// Process supervises a single xemu invocation. It owns the xemu.toml it
// writes, the OS process, and the waiter goroutine that catches unexpected
// exits. Callers construct it per-session and call Start once, Close once.
//
// Lifecycle:
//
//	p := Process{Conf: ..., Display: ":100", Log: ..., OnUnexpectedExit: fn}
//	p.Start()    // spawns xemu, returns once pid is alive
//	... running ...
//	p.Close()    // SIGTERM with 2s grace, then SIGKILL
//
// On an unexpected exit (xemu crashes or is SIGKILLed from outside),
// OnUnexpectedExit fires *once* after the wait returns. Close() suppresses
// the callback because the exit is requested in that path.
type Process struct {
	// Conf carries BIOS paths, binary path, display string, dimensions.
	Conf config.XemuConfig
	// Display overrides Conf.XvfbDisplay when set (lets callers reuse the
	// same config across sessions with distinct displays).
	Display string
	// PreloadPath is the path to videocap_preload.so. When set, LD_PRELOAD
	// is pushed into xemu's env so the shim captures frames via glXSwapBuffers.
	PreloadPath string
	// VideocapSock is the Unix socket the preload shim will connect to.
	// Required when PreloadPath is set; exported via CLOUDPLAY_VIDEOCAP_SOCKET.
	VideocapSock string
	// PulseServer, if non-empty, is pushed as PULSE_SERVER so xemu's SDL
	// audio backend connects to a specific pulse/pipewire-pulse instance.
	// When empty, SDL_AUDIODRIVER=dummy is set (Phase-2 behavior).
	PulseServer string
	// PulseRuntimeDir mirrors XDG_RUNTIME_DIR for the pulse client lookup
	// (parec/pactl peer this way). Required when PulseServer is set.
	PulseRuntimeDir string
	// Log receives lifecycle + stdout/stderr.
	Log *logger.Logger
	// OnUnexpectedExit is invoked from the waiter goroutine when xemu dies
	// without a preceding Close(). Optional.
	OnUnexpectedExit func(err error)

	cmd      *exec.Cmd
	started  atomic.Bool
	closing  atomic.Bool
	waitCh   chan struct{}
	tomlPath string
	tomlDir  string
	onceExit sync.Once
}

// Start writes xemu.toml, spawns Xvfb-dependent xemu, and returns.
// It does *not* wait for the Xbox dashboard to boot — that's a matter of
// video callbacks in Phase 3.
func (p *Process) Start() error {
	if p.started.Load() {
		return fmt.Errorf("xemu: already started")
	}
	bin := p.Conf.BinaryPath
	if bin == "" {
		bin = "xemu"
	}
	display := p.Display
	if display == "" {
		display = p.Conf.XvfbDisplay
	}
	if display == "" {
		return fmt.Errorf("xemu: no display configured")
	}
	if p.Conf.BiosPath == "" {
		return fmt.Errorf("xemu: BiosPath is required")
	}

	flash, err := findBiosFile(p.Conf.BiosPath, "bios", ".bin")
	if err != nil {
		return fmt.Errorf("xemu: flash bios: %w", err)
	}
	boot, err := findBiosFile(p.Conf.BiosPath, "mcpx", ".bin")
	if err != nil {
		return fmt.Errorf("xemu: mcpx bootrom: %w", err)
	}
	hdd, err := findBiosFile(p.Conf.BiosPath, "hdd", ".qcow2")
	if err != nil {
		return fmt.Errorf("xemu: hdd image: %w", err)
	}

	if err := p.writeConfig(flash, boot, hdd); err != nil {
		return err
	}

	p.cmd = exec.Command(bin)
	env := append(os.Environ(), "DISPLAY="+display)
	if p.PulseServer != "" && p.PulseRuntimeDir != "" {
		env = append(env,
			"SDL_AUDIODRIVER=pulse",
			"PULSE_SERVER="+p.PulseServer,
			"XDG_RUNTIME_DIR="+p.PulseRuntimeDir,
		)
	} else {
		// Headless no-audio path — used when AudioCapture is disabled.
		env = append(env, "SDL_AUDIODRIVER=dummy")
	}
	if p.PreloadPath != "" {
		env = append(env, "LD_PRELOAD="+p.PreloadPath)
		if p.VideocapSock != "" {
			env = append(env, "CLOUDPLAY_VIDEOCAP_SOCKET="+p.VideocapSock)
		}
	}
	p.cmd.Env = env
	p.cmd.Stdout = newStreamLogger(p.Log, "[XEMU-PROC] ")
	p.cmd.Stderr = newStreamLogger(p.Log, "[XEMU-PROC] ")
	// New process group so we can signal xemu + any child threads cleanly.
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := p.cmd.Start(); err != nil {
		p.cleanupToml()
		return fmt.Errorf("xemu: start: %w", err)
	}
	p.started.Store(true)
	p.waitCh = make(chan struct{})
	p.Log.Info().Int("pid", p.cmd.Process.Pid).Str("display", display).
		Str("flash", flash).Str("boot", boot).Str("hdd", hdd).
		Str("preload", p.PreloadPath).Str("videocap_sock", p.VideocapSock).
		Msg("[XEMU-PROC] started")

	go p.waitLoop()
	return nil
}

func (p *Process) waitLoop() {
	err := p.cmd.Wait()
	p.started.Store(false)
	if p.closing.Load() {
		p.Log.Info().Msg("[XEMU-PROC] stopped (requested)")
	} else {
		p.Log.Error().Err(err).Msg("[XEMU-PROC] unexpected exit")
		p.onceExit.Do(func() {
			if p.OnUnexpectedExit != nil {
				p.OnUnexpectedExit(err)
			}
		})
	}
	close(p.waitCh)
	p.cleanupToml()
}

// Close stops xemu. Safe to call multiple times and from the OnUnexpectedExit
// callback (guarded by atomic flags). Blocks until the process is reaped.
func (p *Process) Close() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if !p.closing.CompareAndSwap(false, true) {
		// another caller already initiated Close; join them on the wait.
		<-p.waitCh
		return nil
	}
	if !p.started.Load() {
		// already exited on its own; nothing to kill but we still wait.
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

// Pid reports the OS pid of the running xemu, or 0 when not running.
func (p *Process) Pid() int {
	if p.cmd == nil || p.cmd.Process == nil || !p.started.Load() {
		return 0
	}
	return p.cmd.Process.Pid
}

// writeConfig drops a minimal xemu.toml pointing at the configured BIOS
// files. xemu always reads its config from $HOME/.local/share/xemu/xemu/
// (or the per-user equivalent), so we target that path rather than using
// a --config-path flag. A process crash leaves the config in place; the
// next Start() will overwrite it.
func (p *Process) writeConfig(flash, boot, hdd string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("xemu: resolve HOME: %w", err)
	}
	dir := filepath.Join(home, ".local", "share", "xemu", "xemu")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("xemu: mkdir %s: %w", dir, err)
	}
	p.tomlDir = dir
	p.tomlPath = filepath.Join(dir, "xemu.toml")
	body := fmt.Sprintf(`[general]
show_welcome = false
screenshot_dir = ""

[general.updates]
check = false

[sys]
mem_limit = "64"

[sys.files]
bootrom_path = %q
flashrom_path = %q
hdd_path = %q
eeprom_path = ""
`, boot, flash, hdd)
	return os.WriteFile(p.tomlPath, []byte(body), 0o644)
}

func (p *Process) cleanupToml() {
	// Leave the toml in place — xemu's eeprom.bin lives next to it and we
	// want saves/settings to persist across sessions. Only clear the path
	// variable so Close is idempotent in that regard.
	_ = p.tomlPath
}

// findBiosFile returns the first file under biosRoot/<subdir> matching the
// given extension. We glob rather than hardcode names because the community
// names the Complex / Xbox-BIOS / Chihiro dumps differently.
func findBiosFile(biosRoot, subdir, ext string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(biosRoot, subdir, "*"+ext))
	if err != nil {
		return "", err
	}
	for _, m := range matches {
		info, err := os.Stat(m)
		if err == nil && !info.IsDir() && info.Size() > 0 {
			return m, nil
		}
	}
	return "", fmt.Errorf("no %s file found under %s/%s", ext, biosRoot, subdir)
}

// --- stream logger ----------------------------------------------------------

type streamLogger struct {
	log    *logger.Logger
	prefix string
	mu     sync.Mutex
	buf    bytes.Buffer
}

func newStreamLogger(log *logger.Logger, prefix string) io.Writer {
	return &streamLogger{log: log, prefix: prefix}
}

func (s *streamLogger) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, _ := s.buf.Write(p)
	for {
		line, err := s.buf.ReadString('\n')
		if err != nil {
			// put the partial back and wait for more
			s.buf.Reset()
			s.buf.WriteString(line)
			return n, nil
		}
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			s.log.Info().Msgf("%s%s", s.prefix, line)
		}
	}
}

// Compile-time assertions that we satisfy io.Writer and bufio.ReadWriter
// isn't needed; these serve as hints if somebody refactors the stream wiring.
var (
	_ io.Writer = (*streamLogger)(nil)
	_           = bufio.NewReader // keep import if future line-oriented parsing needs it
)

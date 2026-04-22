package flycast

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/giongto35/cloud-game/v3/pkg/config"
)

// writeFlycastConfig drops a minimal emu.cfg at the caller's chosen ConfigDir
// (falling back to "$HOME/.config/flycast/"). Flycast reads this INI on
// startup and persists user-driven changes back to it; we template it fresh
// each Start so the headless cage has deterministic video/audio/input
// settings regardless of what a prior session may have written.
//
// Key settings:
//   - rend.Resolution: capped at the Xvfb screen; higher values force
//     internal scaling and break x11grab expectations.
//   - window.fullscreen=yes: removes the menu bar from the captured frame.
//   - AudioBackend=pulse: routes SDL2 audio into our PipeWire-Pulse bridge.
//   - MapleMainDevices/MapleExpansionDevices: auto-bind via SDL2 to the
//     uinput pads the cage opens before flycast starts. Port A mic slot
//     is left as VMU for Phase 2; Phase 5 rewrites this for Seaman.
func writeFlycastConfig(conf config.FlycastConfig) error {
	dir := conf.ConfigDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("flycast: resolve HOME: %w", err)
		}
		dir = filepath.Join(home, ".config", "flycast")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("flycast: mkdir %s: %w", dir, err)
	}
	biosBlock := ""
	if conf.BiosPath != "" {
		biosBlock = fmt.Sprintf("Dreamcast.HLE_BIOS = no\nDreamcast.BiosPath = %s\n", conf.BiosPath)
	} else {
		biosBlock = "Dreamcast.HLE_BIOS = yes\n"
	}
	// Port A expansion slots: slot 0 is always VMU (ID 1) for saves; slot 1
	// is VMU by default but flipped to the mic device ID when Mic=true.
	// Both slots live on the same controller port; Seaman keys off slot 1.
	slot1 := 1
	if conf.Mic {
		slot1 = conf.MicDeviceID
		if slot1 <= 0 {
			slot1 = 3
		}
	}
	body := fmt.Sprintf(`[config]
Dreamcast.Broadcast = 0
Dreamcast.Cable = 3
Dreamcast.Language = 1
Dreamcast.Region = 1
Dreamcast.ContentPath = /
%sDreamcast.RTC = 0

[audio]
backend = pulse
DisableSound = no

[input]
MapleMainDevices = 0 = 0
MapleExpansionDevices = 0/0 = 1
MapleExpansionDevices = 0/1 = %d

[window]
fullscreen = yes
width = %d
height = %d
rend.Resolution = %d

[video]
rend = OpenGL
`, biosBlock, slot1, conf.Width, conf.Height, conf.Height)
	return os.WriteFile(filepath.Join(dir, "emu.cfg"), []byte(body), 0o644)
}

// buildFlycastEnv composes the environment flycast runs with. DISPLAY routes
// SDL video to our Xvfb; PULSE_SERVER + XDG_RUNTIME_DIR route SDL audio to
// the per-session PipeWire-Pulse bridge when audio capture is enabled;
// otherwise SDL_AUDIODRIVER=dummy gives flycast a no-op audio device.
// XDG_CONFIG_HOME points flycast at our templated emu.cfg. micSource, when
// non-empty, is advertised via PULSE_SOURCE so flycast's SDL mic capture
// picks up our in-process VirtualMicSource by name.
func buildFlycastEnv(display, pulseServer, pulseRuntimeDir, configDir, micSource string) []string {
	env := append(os.Environ(), "DISPLAY="+display)
	if configDir != "" {
		env = append(env, "XDG_CONFIG_HOME="+filepath.Dir(configDir))
	}
	if pulseServer != "" && pulseRuntimeDir != "" {
		env = append(env,
			"SDL_AUDIODRIVER=pulse",
			"PULSE_SERVER="+pulseServer,
			"XDG_RUNTIME_DIR="+pulseRuntimeDir,
		)
		if micSource != "" {
			env = append(env, "PULSE_SOURCE="+micSource)
		}
	} else {
		env = append(env, "SDL_AUDIODRIVER=dummy")
	}
	return env
}

package xemu

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/giongto35/cloud-game/v3/pkg/config"
)

// findBiosFile returns the first file under biosRoot/<subdir> matching the
// given extension. We glob rather than hardcode names because community
// dumps (Complex / Xbox-BIOS / Chihiro) name the files differently.
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

// writeXemuConfig drops a minimal xemu.toml pointing at the configured BIOS
// files. xemu always reads its config from $HOME/.local/share/xemu/xemu/,
// so we target that path rather than using a --config-path flag. A crash
// leaves the config in place; the next writeXemuConfig overwrites it.
//
// fullscreen_on_startup hides xemu's ImGui menu bar so our x11grab capture
// sees only the emulated framebuffer instead of 19px of GUI chrome.
// All four ports are pinned to the stable SDL joystick GUID our uinput pads
// advertise: bustype=USB (0003), vid 0x045E, pid 0x028E — SDL's standard
// signature for the Xbox 360 pad.
func writeXemuConfig(conf config.XemuConfig, dvd string) error {
	flash, err := findBiosFile(conf.BiosPath, "bios", ".bin")
	if err != nil {
		return fmt.Errorf("xemu: flash bios: %w", err)
	}
	boot, err := findBiosFile(conf.BiosPath, "mcpx", ".bin")
	if err != nil {
		return fmt.Errorf("xemu: mcpx bootrom: %w", err)
	}
	hdd, err := findBiosFile(conf.BiosPath, "hdd", ".qcow2")
	if err != nil {
		return fmt.Errorf("xemu: hdd image: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("xemu: resolve HOME: %w", err)
	}
	dir := filepath.Join(home, ".local", "share", "xemu", "xemu")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("xemu: mkdir %s: %w", dir, err)
	}
	tomlPath := filepath.Join(dir, "xemu.toml")
	dvdLine := ""
	if dvd != "" {
		dvdLine = fmt.Sprintf("dvd_path = %q\n", dvd)
	}
	body := fmt.Sprintf(`[general]
show_welcome = false
screenshot_dir = ""
fullscreen_on_startup = true

[general.updates]
check = false

[sys]
mem_limit = "64"

[sys.files]
bootrom_path = %q
flashrom_path = %q
hdd_path = %q
%seeprom_path = ""

[input]
auto_bind = true

[input.bindings]
port1 = "030000005e0400008e02000010010000"
port2 = "030000005e0400008e02000010010000"
port3 = "030000005e0400008e02000010010000"
port4 = "030000005e0400008e02000010010000"
`, boot, flash, hdd, dvdLine)
	return os.WriteFile(tomlPath, []byte(body), 0o644)
}

// buildXemuEnv composes the environment xemu runs with. DISPLAY routes SDL
// video to our Xvfb; PULSE_SERVER + XDG_RUNTIME_DIR route SDL audio to the
// per-session PipeWire-Pulse bridge when audio capture is enabled; otherwise
// SDL_AUDIODRIVER=dummy gives xemu a no-op audio device.
func buildXemuEnv(display, pulseServer, pulseRuntimeDir string) []string {
	env := append(os.Environ(), "DISPLAY="+display)
	if pulseServer != "" && pulseRuntimeDir != "" {
		env = append(env,
			"SDL_AUDIODRIVER=pulse",
			"PULSE_SERVER="+pulseServer,
			"XDG_RUNTIME_DIR="+pulseRuntimeDir,
		)
	} else {
		env = append(env, "SDL_AUDIODRIVER=dummy")
	}
	return env
}

package config

// XemuConfig controls the xemu native-emulator backend.
//
// When Enabled=false (the default) the worker never spawns xemu and any game
// flagged as backend=xemu fails at HandleGameStart. The xemu backend is
// linux-only; see pkg/worker/caged/xemu.
type XemuConfig struct {
	// Enabled gates whether Manager.Load("xemu", ...) initializes the backend.
	Enabled bool

	// BinaryPath is the xemu executable path inside the worker container.
	// Empty falls back to PATH lookup.
	BinaryPath string `yaml:"binaryPath"`

	// BiosPath is the directory containing MCPX + flash BIOS dumps (mcpx.bin,
	// bios.bin, eeprom.bin). xemu refuses to boot without these. Bind-mounted
	// into the container; proprietary so never committed.
	BiosPath string `yaml:"biosPath"`

	// XvfbDisplay is the X display xemu renders into. Convention: one worker =
	// one display = one session. Defaults to ":100".
	XvfbDisplay string `yaml:"xvfbDisplay"`

	// Width/Height are the fixed framebuffer dimensions for Phase 1. Dynamic
	// resolution negotiation will come in a later phase.
	Width  int
	Height int

	// VideoPreloadPath points at the compiled videocap_preload.so used by
	// Phase 3's LD_PRELOAD-based GL capture path. Empty disables capture
	// (xemu runs but no frames leave the process — stub-emitter stays live).
	VideoPreloadPath string `yaml:"videoPreloadPath"`

	// AudioCapture toggles Phase-4's PipeWire/pulseaudio capture path.
	// When true, Caged.Start spawns a private pipewire session and a
	// parec subprocess that feeds app.Audio chunks into the configured
	// callback. xemu is run with SDL_AUDIODRIVER=pulse pointing at the
	// private session's socket.
	AudioCapture bool `yaml:"audioCapture"`

	// DvdPath, when non-empty, is written into xemu.toml's [sys.files]
	// dvd_path so xemu mounts this ISO as the DVD at boot. Phase-6 will
	// make this per-game dynamic via the library metadata; for Phase 4
	// testing it lets the harness point xemu at a homebrew ISO.
	DvdPath string `yaml:"dvdPath"`

	// InputInject toggles Phase-5's uinput virtual Xbox 360 gamepad.
	// When true, Caged.Start creates a /dev/uinput device that SDL-hotplug
	// surfaces to xemu, and the cage's Input(port, device, data) forwards
	// packets into it. Requires /dev/uinput access (quadlet: AddDevice +
	// GroupAdd=input).
	InputInject bool `yaml:"inputInject"`
}

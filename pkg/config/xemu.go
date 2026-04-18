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
}

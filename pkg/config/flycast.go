package config

// FlycastConfig controls the flycast native-emulator backend (Dreamcast).
//
// When Enabled=false (the default) the worker never spawns flycast and any
// game flagged as backend=flycast fails at HandleGameStart. The flycast
// native backend is linux-only; see pkg/worker/caged/flycast.
type FlycastConfig struct {
	// Enabled gates whether Manager.Load("flycast", ...) initializes the backend.
	Enabled bool

	// BinaryPath is the flycast executable path inside the worker container.
	// Empty falls back to PATH lookup.
	BinaryPath string `yaml:"binaryPath"`

	// BiosPath is the directory containing dc_boot.bin / dc_flash.bin.
	// Optional — many ROMs run without BIOS (flycast synthesizes a HLE
	// bootrom). Seaman and a handful of others need the real BIOS.
	// Bind-mounted into the container; proprietary so never committed.
	BiosPath string `yaml:"biosPath"`

	// ConfigDir is the directory flycast reads emu.cfg from. Defaults to
	// "$HOME/.config/flycast". The caged adapter writes emu.cfg here at
	// Start so fresh container mounts get a deterministic configuration.
	ConfigDir string `yaml:"configDir"`

	// XvfbDisplay is the X display flycast renders into. One worker = one
	// display = one session. Defaults to ":110" (:100 is xemu's).
	XvfbDisplay string `yaml:"xvfbDisplay"`

	// Width/Height are the fixed framebuffer dimensions for Phase 2.
	// 640x480 matches Dreamcast native output.
	Width  int
	Height int

	// AudioCapture toggles the PipeWire/pulseaudio capture path. When true,
	// Caged.Start spawns a private pipewire session and a parec subprocess
	// that feeds app.Audio chunks into the configured callback. Flycast is
	// run with SDL_AUDIODRIVER=pulse pointing at the private session.
	AudioCapture bool `yaml:"audioCapture"`

	// RomPath, when non-empty, is the Dreamcast ROM flycast loads on the
	// next Start. HandleGameStart resolves the library ROM path and sets
	// this before StartApp; tests can set it directly.
	RomPath string `yaml:"romPath"`

	// InputInject toggles the uinput virtual gamepad. When true, Caged.Start
	// creates /dev/uinput devices that SDL-hotplug surfaces to flycast, and
	// the cage's Input(port, device, data) forwards packets into them.
	// Requires /dev/uinput access.
	InputInject bool `yaml:"inputInject"`

	// Mic toggles the microphone uplink path for Seaman and other DC titles
	// that use the microphone peripheral. When true (and AudioCapture is
	// also true — the mic path piggybacks on the per-session PipeWire
	// instance), Caged.Start loads a module-pipe-source named by
	// MicSourceName and templates emu.cfg to bind Port A expansion slot 2
	// to the flycast Microphone maple device (MicDeviceID). The browser's
	// "microphone" WebRTC data channel delivers S16LE mono PCM chunks to
	// the source's FIFO.
	Mic bool `yaml:"mic"`

	// MicSourceName is the PulseAudio source name the in-process
	// VirtualMicSource advertises and that flycast opens via PULSE_SOURCE.
	// Defaults to "cloudplay-mic".
	MicSourceName string `yaml:"micSourceName"`

	// MicRate is the sample rate of the uplink PCM. 11025 matches the
	// Dreamcast microphone's native rate; browser-side AudioWorklet
	// resamples from the AudioContext rate (typically 48 kHz).
	MicRate int `yaml:"micRate"`

	// MicDeviceID is the flycast maple-expansion device ID to bind to Port
	// A slot 2 when Mic is true. Default 3 matches the commonly-reported
	// flycast Microphone value; make configurable since the enum has
	// drifted across flycast versions.
	MicDeviceID int `yaml:"micDeviceId"`
}

package app

// InputDevice identifies the kind of input carried in an
// app.App.Input(port, device, data) call. It is transported on the
// interface as a bare byte, but adapters and the coordinator-side channel
// wiring reference these named constants so the wire format can't drift
// silently between layers.
//
// Values are stable: changing them breaks cross-layer contract with the
// browser (keyboard/mouse/microphone data channels) and with the legacy
// libretro Device enum in pkg/worker/caged/libretro/nanoarch/input.go
// which historically defined 0/1/2.
type InputDevice = byte

const (
	InputRetroPad   InputDevice = 0
	InputKeyboard   InputDevice = 1
	InputMouse      InputDevice = 2
	// InputMicrophone carries uplink PCM from the browser to an adapter
	// that knows how to deliver it to the emulator (flycast → VirtualMicSource
	// → PipeWire pipe-source). Other adapters ignore it.
	InputMicrophone InputDevice = 3
)

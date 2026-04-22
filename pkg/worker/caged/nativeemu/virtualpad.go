package nativeemu

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

var filepathGlob = filepath.Glob

// deviceNameOf opens /dev/input/eventN RDONLY and queries EVIOCGNAME.
// Returns "" on any error.
func deviceNameOf(path string) string {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer syscall.Close(fd)
	const evBase = 'E'
	// _IOC(_IOC_READ=2, type, nr, size): (size<<16) | (type<<8) | nr | (2<<30)
	req := uintptr((2 << 30) | (256 << 16) | (evBase << 8) | 0x06)
	var buf [256]byte
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return ""
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n])
}

// VirtualPad is a uinput-backed gamepad. The default configuration advertises
// a Microsoft Xbox 360 controller (vid 0x045E/pid 0x028E with the xpad quirk
// button codes) because that's what every SDL2-based emulator in scope
// (xemu, flycast, etc.) recognizes out of the box via its bundled
// gamecontrollerdb mapping.
//
// The injected packet format matches the libretro RetroPad wire format the
// cloudplay frontend already sends:
//
//	bytes 0-1   uint16 LE   buttons bitmask (libretro RETRO_DEVICE_ID_JOYPAD_*)
//	bytes 2-3   int16  LE   left stick X
//	bytes 4-5   int16  LE   left stick Y
//	bytes 6-7   int16  LE   right stick X
//	bytes 8-9   int16  LE   right stick Y
//	bytes 10-11 int16  LE   analog left trigger (0..32767 typical)
//	bytes 12-13 int16  LE   analog right trigger
//
// Note the B/A and Y/X swaps between libretro (Nintendo convention) and Xbox
// (A south, B east, X west, Y north) — we emit xpad-style quirky codes
// (BTN_X = BTN_NORTH, BTN_Y = BTN_WEST) to match SDL's built-in mapping for
// the real 360 pad.
type VirtualPad struct {
	// Log receives lifecycle diagnostics.
	Log *logger.Logger
	// LogPrefix tags every log line. Defaults to "[NATIVE-INPUT] " when empty.
	LogPrefix string
	// DeviceName is the kernel-visible input device name. Defaults to
	// "Microsoft X-Box 360 pad" so SDL picks up the canonical mapping.
	DeviceName string
	// Port is the 0-indexed player port this pad represents — used in logs only.
	Port int

	fd    int
	open  bool
	mu    sync.Mutex
	state padState
}

type padState struct {
	buttons uint16
	lx, ly  int16
	rx, ry  int16
	lt, rt  int16
	hatX    int8
	hatY    int8
}

func (p *VirtualPad) logPrefix() string {
	if p.LogPrefix == "" {
		return "[NATIVE-INPUT] "
	}
	return p.LogPrefix
}

// --- uinput ioctl numbers (static; architecture-independent on Linux) ------

const (
	uiDevCreate  = 0x5501
	uiDevDestroy = 0x5502
	uiDevSetup   = 0x405C5503 // _IOW('U', 3, uinput_setup[92])
	uiAbsSetup   = 0x401C5504 // _IOW('U', 4, uinput_abs_setup[28])
	uiSetEvbit   = 0x40045564 // _IOW('U', 100, int)
	uiSetKeybit  = 0x40045565 // _IOW('U', 101, int)
	uiSetAbsbit  = 0x40045567 // _IOW('U', 103, int)

	evSyn     = 0x00
	evKey     = 0x01
	evAbs     = 0x03
	synReport = 0x00

	btnA      = 0x130 // south
	btnB      = 0x131 // east
	btnX      = 0x133 // north (xpad-style)
	btnY      = 0x134 // west  (xpad-style)
	btnTL     = 0x136
	btnTR     = 0x137
	btnSelect = 0x13A
	btnStart  = 0x13B
	btnMode   = 0x13C
	btnThumbL = 0x13D
	btnThumbR = 0x13E

	absX     = 0x00
	absY     = 0x01
	absZ     = 0x02 // LT
	absRX    = 0x03
	absRY    = 0x04
	absRZ    = 0x05 // RT
	absHat0X = 0x10
	absHat0Y = 0x11

	busUSB = 0x03
)

// Libretro RETRO_DEVICE_ID_JOYPAD_* bit indices in the buttons field.
const (
	bitB      = 0 // libretro B = south = Xbox A
	bitY      = 1 // libretro Y = west  = Xbox X
	bitSelect = 2
	bitStart  = 3
	bitUp     = 4
	bitDown   = 5
	bitLeft   = 6
	bitRight  = 7
	bitA      = 8 // libretro A = east  = Xbox B
	bitX      = 9 // libretro X = north = Xbox Y
	bitL      = 10
	bitR      = 11
	bitL2     = 12
	bitR2     = 13
	bitL3     = 14
	bitR3     = 15
)

// --- struct layouts matching <linux/uinput.h> -------------------------------

type inputID struct {
	Bustype uint16
	Vendor  uint16
	Product uint16
	Version uint16
}

type uinputSetup struct {
	ID           inputID
	Name         [80]byte
	FFEffectsMax uint32
}

type inputAbsinfo struct {
	Value      int32
	Minimum    int32
	Maximum    int32
	Fuzz       int32
	Flat       int32
	Resolution int32
}

// uinputAbsSetup: 2 bytes code + 2 bytes padding + 24 bytes absinfo = 28 bytes.
type uinputAbsSetup struct {
	Code    uint16
	_       uint16
	AbsInfo inputAbsinfo
}

// inputEvent mirrors struct input_event on x86_64. timeval is 16 bytes; we
// leave both zeroed — the kernel stamps real time itself when reading.
type inputEvent struct {
	TimeSec  int64
	TimeUsec int64
	Type     uint16
	Code     uint16
	Value    int32
}

// --- device bring-up --------------------------------------------------------

// Open creates the uinput device and waits briefly for udev to settle.
func (p *VirtualPad) Open() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open {
		return fmt.Errorf("virtualpad: already open")
	}
	name := p.DeviceName
	if name == "" {
		name = "Microsoft X-Box 360 pad"
	}

	fd, err := syscall.Open("/dev/uinput", syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("virtualpad: open /dev/uinput: %w", err)
	}

	for _, ev := range []int{evKey, evAbs, evSyn} {
		if err := ioctlInt(fd, uiSetEvbit, ev); err != nil {
			_ = syscall.Close(fd)
			return fmt.Errorf("virtualpad: UI_SET_EVBIT(%d): %w", ev, err)
		}
	}

	keys := []int{
		btnA, btnB, btnX, btnY,
		btnTL, btnTR,
		btnSelect, btnStart, btnMode,
		btnThumbL, btnThumbR,
	}
	for _, k := range keys {
		if err := ioctlInt(fd, uiSetKeybit, k); err != nil {
			_ = syscall.Close(fd)
			return fmt.Errorf("virtualpad: UI_SET_KEYBIT(0x%x): %w", k, err)
		}
	}

	stickAxes := []int{absX, absY, absRX, absRY}
	triggerAxes := []int{absZ, absRZ}
	hatAxes := []int{absHat0X, absHat0Y}
	for _, a := range append(append(stickAxes, triggerAxes...), hatAxes...) {
		if err := ioctlInt(fd, uiSetAbsbit, a); err != nil {
			_ = syscall.Close(fd)
			return fmt.Errorf("virtualpad: UI_SET_ABSBIT(%d): %w", a, err)
		}
	}

	var setup uinputSetup
	setup.ID = inputID{Bustype: busUSB, Vendor: 0x045E, Product: 0x028E, Version: 0x0110}
	copy(setup.Name[:], name)
	if err := ioctlPtr(fd, uiDevSetup, unsafe.Pointer(&setup)); err != nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("virtualpad: UI_DEV_SETUP: %w", err)
	}

	for _, a := range stickAxes {
		if err := setupAxis(fd, a, -32768, 32767, 16, 128); err != nil {
			_ = syscall.Close(fd)
			return err
		}
	}
	for _, a := range triggerAxes {
		if err := setupAxis(fd, a, 0, 255, 0, 0); err != nil {
			_ = syscall.Close(fd)
			return err
		}
	}
	for _, a := range hatAxes {
		if err := setupAxis(fd, a, -1, 1, 0, 0); err != nil {
			_ = syscall.Close(fd)
			return err
		}
	}

	if err := ioctlInt(fd, uiDevCreate, 0); err != nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("virtualpad: UI_DEV_CREATE: %w", err)
	}
	// Give udev a moment to assign an /dev/input/eventN before SDL2 consumers
	// race on a device that isn't yet enumerable.
	time.Sleep(100 * time.Millisecond)

	p.fd = fd
	p.open = true
	p.Log.Info().Int("port", p.Port).Str("name", name).Int("fd", fd).
		Msgf("%svirtual pad created", p.logPrefix())
	return nil
}

// DevicePath discovers the /dev/input/eventN path for this virtual pad by
// scanning /dev/input/ for a node whose EVIOCGNAME matches our DeviceName.
// Returns "" if nothing matches yet (udev can lag UI_DEV_CREATE).
func (p *VirtualPad) DevicePath() string {
	name := p.DeviceName
	if name == "" {
		name = "Microsoft X-Box 360 pad"
	}
	entries, err := filepathGlob("/dev/input/event*")
	if err != nil {
		return ""
	}
	for _, path := range entries {
		if deviceNameOf(path) == name {
			return path
		}
	}
	return ""
}

// Close destroys the uinput device.
func (p *VirtualPad) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.open {
		return nil
	}
	_ = ioctlInt(p.fd, uiDevDestroy, 0)
	err := syscall.Close(p.fd)
	p.open = false
	p.Log.Info().Int("port", p.Port).Msgf("%svirtual pad destroyed", p.logPrefix())
	return err
}

// Inject parses one RetroPad packet and emits every evdev event needed to
// bring the kernel's view of the pad into the packet's state. Only changed
// fields are emitted so an idle stream turns into a handful of SYN events
// rather than hundreds of EV_* per second.
func (p *VirtualPad) Inject(data []byte) error {
	if len(data) < 14 {
		return fmt.Errorf("virtualpad: packet too short: %d bytes", len(data))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.open {
		return fmt.Errorf("virtualpad: not open")
	}
	btns := binary.LittleEndian.Uint16(data[0:2])
	lx := int16(binary.LittleEndian.Uint16(data[2:4]))
	ly := int16(binary.LittleEndian.Uint16(data[4:6]))
	rx := int16(binary.LittleEndian.Uint16(data[6:8]))
	ry := int16(binary.LittleEndian.Uint16(data[8:10]))
	lt := int16(binary.LittleEndian.Uint16(data[10:12]))
	rt := int16(binary.LittleEndian.Uint16(data[12:14]))

	// D-pad bits fold into HAT axes. Simultaneous UP+DOWN → UP wins
	// (matches xpad quirk); same for LEFT+RIGHT.
	var hatX, hatY int8
	switch {
	case btns&(1<<bitLeft) != 0:
		hatX = -1
	case btns&(1<<bitRight) != 0:
		hatX = 1
	}
	switch {
	case btns&(1<<bitUp) != 0:
		hatY = -1
	case btns&(1<<bitDown) != 0:
		hatY = 1
	}

	var events []inputEvent
	add := func(typ, code uint16, val int32) {
		events = append(events, inputEvent{Type: typ, Code: code, Value: val})
	}

	type btnMap struct {
		bit  uint16
		code uint16
	}
	btnDiff := func(prev, cur uint16, m btnMap) {
		pm := prev & (1 << m.bit)
		cm := cur & (1 << m.bit)
		if pm == cm {
			return
		}
		v := int32(0)
		if cm != 0 {
			v = 1
		}
		add(evKey, m.code, v)
	}
	for _, m := range []btnMap{
		{bitB, btnA}, {bitA, btnB}, {bitY, btnX}, {bitX, btnY},
		{bitL, btnTL}, {bitR, btnTR},
		{bitSelect, btnSelect}, {bitStart, btnStart},
		{bitL3, btnThumbL}, {bitR3, btnThumbR},
	} {
		btnDiff(p.state.buttons, btns, m)
	}

	axisDiff := func(prev, cur int16, code uint16) {
		if prev == cur {
			return
		}
		add(evAbs, code, int32(cur))
	}
	axisDiff(p.state.lx, lx, absX)
	axisDiff(p.state.ly, ly, absY)
	axisDiff(p.state.rx, rx, absRX)
	axisDiff(p.state.ry, ry, absRY)

	// Triggers: scale libretro's int16 (typically 0..32767) down to 0..255.
	mapTrig := func(v int16) int32 {
		if v < 0 {
			return 0
		}
		return int32(v) * 255 / 32767
	}
	oldLT := mapTrig(p.state.lt)
	newLT := mapTrig(lt)
	if oldLT != newLT {
		add(evAbs, absZ, newLT)
	}
	oldRT := mapTrig(p.state.rt)
	newRT := mapTrig(rt)
	if oldRT != newRT {
		add(evAbs, absRZ, newRT)
	}

	if p.state.hatX != hatX {
		add(evAbs, absHat0X, int32(hatX))
	}
	if p.state.hatY != hatY {
		add(evAbs, absHat0Y, int32(hatY))
	}

	if len(events) == 0 {
		return nil
	}
	events = append(events, inputEvent{Type: evSyn, Code: synReport, Value: 0})

	buf := make([]byte, len(events)*int(unsafe.Sizeof(inputEvent{})))
	for i, e := range events {
		off := i * int(unsafe.Sizeof(inputEvent{}))
		packInputEvent(buf[off:], e)
	}
	n, err := syscall.Write(p.fd, buf)
	if err != nil {
		return fmt.Errorf("virtualpad: write: %w", err)
	}
	if n != len(buf) {
		return fmt.Errorf("virtualpad: short write %d/%d", n, len(buf))
	}

	p.state.buttons = btns
	p.state.lx, p.state.ly = lx, ly
	p.state.rx, p.state.ry = rx, ry
	p.state.lt, p.state.rt = lt, rt
	p.state.hatX, p.state.hatY = hatX, hatY
	return nil
}

func packInputEvent(dst []byte, e inputEvent) {
	binary.LittleEndian.PutUint64(dst[0:8], uint64(e.TimeSec))
	binary.LittleEndian.PutUint64(dst[8:16], uint64(e.TimeUsec))
	binary.LittleEndian.PutUint16(dst[16:18], e.Type)
	binary.LittleEndian.PutUint16(dst[18:20], e.Code)
	binary.LittleEndian.PutUint32(dst[20:24], uint32(e.Value))
}

func ioctlInt(fd int, req uintptr, arg int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

func ioctlPtr(fd int, req uintptr, ptr unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(ptr))
	if errno != 0 {
		return errno
	}
	return nil
}

func setupAxis(fd int, code int, min, max, fuzz, flat int32) error {
	setup := uinputAbsSetup{
		Code: uint16(code),
		AbsInfo: inputAbsinfo{
			Minimum: min,
			Maximum: max,
			Fuzz:    fuzz,
			Flat:    flat,
		},
	}
	if err := ioctlPtr(fd, uiAbsSetup, unsafe.Pointer(&setup)); err != nil {
		return fmt.Errorf("virtualpad: UI_ABS_SETUP(%d): %w", code, err)
	}
	return nil
}

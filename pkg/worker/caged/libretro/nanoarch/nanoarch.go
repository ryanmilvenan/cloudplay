package nanoarch

import (
	"errors"
	"fmt"
	"maps"
	stlos "os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/os"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/libretro/graphics"
	"github.com/giongto35/cloud-game/v3/pkg/worker/thread"
)

/*
#include "libretro.h"
#include "nanoarch.h"
#include <stdlib.h>
#include <signal.h>
#include <pthread.h>

// ── SIGBUS recovery handler ────────────────────────────────────────────────
//
// Root cause (confirmed via core dump 2026-03-30):
// dolphin_libretro.so has a race: a background thread calls
// ftruncate(shm_fd, 0) on the GameCube /dev/shm/dolphin-emu.* segment while
// the CPU-emulation thread still has it mmap'd.  This generates
// SIGBUS (si_code=BUS_ADRERR=2) when the CPU thread accesses a page whose
// physical backing has been removed.
//
// Fix: intercept SIGBUS before Go's runtime.  When BUS_ADRERR is detected
// and the faulting address is a user-space virtual address, replace the
// missing page with a fresh anonymous zero page via mmap(MAP_FIXED|MAP_ANON).
// The instruction is then retried; the read/write hits zeros instead of
// crashing.  For unrecognised si_code values the handler re-raises with
// SIG_DFL so unexpected faults are still visible.
//
// The handler is installed in nanoarch_install_sigbus_handler(), called from
// CoreLoad before retro_init, so it is in place before Dolphin creates its
// shm segments.

#include <sys/mman.h>
#include <unistd.h>
#include <string.h>

static struct sigaction g_prev_sigbus_sa;  // saved Go sigaction
static int g_sigbus_handler_installed = 0;

static void sigbus_recovery_handler(int sig, siginfo_t *info, void *uctx) {
    (void)sig;
    (void)uctx;
    if (info == NULL) goto reraise;
    // Only recover from hardware memory-access errors (BUS_ADRERR).
    if (info->si_code != BUS_ADRERR) goto reraise;
    {
        // Map a fresh zero page over the faulting address so the access
        // returns 0 and execution can continue.
        void *fault_page = (void *)((uintptr_t)info->si_addr & ~(uintptr_t)(4096-1));
        void *result = mmap(fault_page, 4096,
                            PROT_READ | PROT_WRITE,
                            MAP_FIXED | MAP_PRIVATE | MAP_ANONYMOUS,
                            -1, 0);
        if (result != MAP_FAILED) return;
    }
reraise:
    {
        struct sigaction sa;
        memset(&sa, 0, sizeof(sa));
        sa.sa_handler = SIG_DFL;
        sigemptyset(&sa.sa_mask);
        sigaction(SIGBUS, &sa, NULL);
    }
    raise(SIGBUS);
}

static void nanoarch_install_sigbus_handler(void) {
    if (g_sigbus_handler_installed) return;
    struct sigaction sa;
    memset(&sa, 0, sizeof(sa));
    sa.sa_sigaction = sigbus_recovery_handler;
    sigemptyset(&sa.sa_mask);
    sa.sa_flags = SA_SIGINFO | SA_ONSTACK | SA_NODEFER;
    sigaction(SIGBUS, &sa, &g_prev_sigbus_sa);
    g_sigbus_handler_installed = 1;
}

// nanoarch_reset_core_signals installs the SIGBUS recovery handler and
// unblocks SIGBUS on the calling thread.
static void nanoarch_reset_core_signals(void) {
    nanoarch_install_sigbus_handler();
    sigset_t unblock;
    sigemptyset(&unblock);
    sigaddset(&unblock, SIGBUS);
    pthread_sigmask(SIG_UNBLOCK, &unblock, NULL);
}
*/
import "C"

var (
	RGBA5551    = PixFmt{C: 0, BPP: 2} // BIT_FORMAT_SHORT_5_5_5_1 has 5 bits R, 5 bits G, 5 bits B, 1 bit alpha
	RGBA8888Rev = PixFmt{C: 1, BPP: 4} // BIT_FORMAT_INT_8_8_8_8_REV has 8 bits R, 8 bits G, 8 bits B, 8 bit alpha
	RGB565      = PixFmt{C: 2, BPP: 2} // BIT_FORMAT_SHORT_5_6_5 has 5 bits R, 6 bits G, 5 bits
)

type Nanoarch struct {
	Handlers

	keyboard KeyboardState
	mouse    MouseState
	retropad InputState

	keyboardCb    *C.struct_retro_keyboard_callback
	LastFrameTime int64
	LibCo         bool
	meta          Metadata
	options       map[string]string
	options4rom   map[string]map[string]string
	reserved      chan struct{} // limits concurrent use
	Rot           uint
	serializeSize C.size_t
	Stopped       atomic.Bool
	sys           struct {
		av  C.struct_retro_system_av_info
		i   C.struct_retro_system_info
		api C.unsigned
	}
	tickTime         int64
	cSaveDirectory   *C.char
	cSystemDirectory *C.char
	cUserName        *C.char
	Video            struct {
		gl struct {
			enabled bool
			autoCtx bool
		}
		hw     *C.struct_retro_hw_render_callback
		PixFmt PixFmt
	}
	// vulkan holds the Vulkan context state.  Populated only when the core
	// requests RETRO_HW_CONTEXT_VULKAN and the `vulkan` build tag is active;
	// otherwise it's an empty zero-value struct (see nanoarch_novulkan.go).
	vulkan                   vulkanState
	vfr                      bool
	Aspect                   bool
	glCtx                    graphics.HeadlessGLContext
	hackSkipHwContextDestroy bool
	hackSkipSameThreadSave   bool
	limiter                  func(func())
	log                      *logger.Logger
}

type Handlers struct {
	OnAudio        func(ptr unsafe.Pointer, frames int)
	OnVideo        func(data []byte, delta int32, fi FrameInfo)
	OnDup          func()
	OnSystemAvInfo func()
	OnRumble       func(port uint, effect uint, strength uint16)
}

type FrameInfo struct {
	W      uint
	H      uint
	Stride uint
}

type Metadata struct {
	FrameDup        bool
	LibPath         string // the full path to some emulator lib
	IsGlAllowed     bool
	UsesLibCo       bool
	AutoGlContext   bool
	HasVFR          bool
	Options         map[string]string
	Options4rom     map[string]map[string]string
	Hacks           []string
	Hid             map[int][]int
	CoreAspectRatio bool
	KbMouseSupport  bool
	LibExt          string
}

type PixFmt struct {
	C   uint32
	BPP uint
}

func (p PixFmt) String() string {
	switch p.C {
	case 0:
		return "RGBA5551/2"
	case 1:
		return "RGBA8888Rev/4"
	case 2:
		return "RGB565/2"
	default:
		return fmt.Sprintf("Unknown (%v/%v)", p.C, p.BPP)
	}
}

// Nan0 is a global link for C callbacks to Go
var Nan0 = Nanoarch{
	reserved: make(chan struct{}, 1), // this thing forbids concurrent use of the emulator
	Stopped:  atomic.Bool{},
	limiter:  func(fn func()) { fn() },
	Handlers: Handlers{
		OnAudio:  func(unsafe.Pointer, int) {},
		OnVideo:  func([]byte, int32, FrameInfo) {},
		OnDup:    func() {},
		OnRumble: func(uint, uint, uint16) {},
	},
}

// init provides a global single instance lock
// !to remove when isolated properly
func init() { Nan0.reserved <- struct{}{} }

func NewNano(localPath string) *Nanoarch {
	nano := &Nan0
	nano.cSaveDirectory = C.CString(localPath + "/legacy_save")
	nano.cSystemDirectory = C.CString(localPath + "/system")
	nano.cUserName = C.CString("retro")
	return nano
}

func (n *Nanoarch) AspectRatio() float32             { return float32(n.sys.av.geometry.aspect_ratio) }
func (n *Nanoarch) AudioSampleRate() int             { return int(n.sys.av.timing.sample_rate) }
func (n *Nanoarch) VideoFramerate() int              { return int(n.sys.av.timing.fps) }
func (n *Nanoarch) IsPortrait() bool                 { return 90 == n.Rot%180 }
func (n *Nanoarch) KbMouseSupport() bool             { return n.meta.KbMouseSupport }
func (n *Nanoarch) BaseWidth() int                   { return int(n.sys.av.geometry.base_width) }
func (n *Nanoarch) BaseHeight() int                  { return int(n.sys.av.geometry.base_height) }
func (n *Nanoarch) WaitReady()                       { <-n.reserved }
func (n *Nanoarch) Close()                           { n.Stopped.Store(true); n.reserved <- struct{}{} }
func (n *Nanoarch) SetLogger(log *logger.Logger)     { n.log = log }
func (n *Nanoarch) SetVideoDebounce(t time.Duration) { n.limiter = NewLimit(t) }
func (n *Nanoarch) SaveDir() string                  { return C.GoString(n.cSaveDirectory) }

// needsJoypadWorkaround returns true for cores whose
// retro_set_controller_port_device is broken for RETRO_DEVICE_ANALOG.
// These cores get RETRO_DEVICE_JOYPAD (1) instead of the system-wide
// RETRO_DEVICE_ANALOG (5) default.
//
// PCSX2: its switch only handles RETRO_DEVICE_JOYPAD → DualShock2.
//        RETRO_DEVICE_ANALOG hits the default case → pad type "None",
//        silently disabling all input.
func needsJoypadWorkaround() bool {
	lib := strings.ToLower(Nan0.meta.LibPath)
	return strings.Contains(lib, "pcsx2")
}

// flipFrameVertical returns a vertically flipped copy of the raw frame.
// OpenGL readback uses a bottom-left origin, while the rest of CloudPlay
// expects top-left. Apply this to GL-fallback cores (Flycast, PCSX2).
func flipFrameVertical(src []byte, width, height, bpp int) []byte {
	if len(src) == 0 || width <= 0 || height <= 0 || bpp <= 0 {
		return src
	}
	row := width * bpp
	if row <= 0 || len(src) < row*height {
		return src
	}
	out := make([]byte, row*height)
	for y := 0; y < height; y++ {
		srcOff := y * row
		dstOff := (height - 1 - y) * row
		copy(out[dstOff:dstOff+row], src[srcOff:srcOff+row])
	}
	return out
}
func (n *Nanoarch) SetSaveDirSuffix(sx string) {
	dir := C.GoString(n.cSaveDirectory) + "/" + sx
	err := os.CheckCreateDir(dir)
	if err != nil {
		n.log.Error().Msgf("couldn't create %v, %v", dir, err)
	}
	if n.cSaveDirectory != nil {
		C.free(unsafe.Pointer(n.cSaveDirectory))
	}
	n.cSaveDirectory = C.CString(dir)
}
func (n *Nanoarch) DeleteSaveDir() error {
	if n.cSaveDirectory == nil {
		return nil
	}

	dir := C.GoString(n.cSaveDirectory)
	return os.RemoveAll(dir)
}

func (n *Nanoarch) CoreLoad(meta Metadata) {
	var err error
	n.meta = meta
	n.LibCo = meta.UsesLibCo
	n.vfr = meta.HasVFR
	n.Aspect = meta.CoreAspectRatio
	n.Video.gl.autoCtx = meta.AutoGlContext
	n.Video.gl.enabled = meta.IsGlAllowed

	// Install a C-level SIGBUS recovery handler before retro_init.
	//
	// Root cause (confirmed via core dump 2026-03-30):
	// dolphin_libretro.so races: a background thread calls ftruncate(shm_fd,0)
	// on the GameCube /dev/shm/dolphin-emu.* segment while the CPU emulation
	// thread (same_thread / run_loop) still has it mmap'd.  Go's runtime
	// intercepts the resulting BUS_ADRERR SIGBUS before any C handler can run,
	// then re-raises it — hitting SIG_DFL and crashing the process.
	//
	// The C helper nanoarch_reset_core_signals() installs a SA_SIGINFO handler
	// that catches BUS_ADRERR and replaces the faulting page with a zero
	// anonymous page (mmap MAP_FIXED|MAP_ANON), allowing the instruction to
	// retry and succeed.  This is installed BEFORE retro_init so it is in
	// place before Dolphin creates its shm regions.
	//
	// We also call signal.Ignore(SIGBUS) here so that Go's runtime stops
	// competing with our C handler: Go will mark SIGBUS as "ignored" in its
	// internal signal table, which prevents Go from reinstalling its own
	// sigaction over our C handler when the Go runtime refreshes signal state.
	if meta.UsesLibCo {
		signal.Ignore(syscall.SIGBUS)
	}
	C.nanoarch_reset_core_signals()

	thread.SwitchGraphics(n.Video.gl.enabled)

	// hacks
	Nan0.hackSkipHwContextDestroy = meta.HasHack("skip_hw_context_destroy")
	Nan0.hackSkipSameThreadSave = meta.HasHack("skip_same_thread_save")

	// reset controllers
	n.retropad = InputState{}
	n.keyboardCb = nil
	n.keyboard = KeyboardState{}
	n.mouse = MouseState{}

	n.options = maps.Clone(meta.Options)
	n.options4rom = meta.Options4rom

	corePath := meta.LibPath + meta.LibExt
	coreLib, err = loadLib(corePath)
	// fallback to sequential lib loader (first successfully loaded)
	if err != nil {
		n.log.Error().Err(err).Msgf("load fail: %v", corePath)
		coreLib, err = loadLibRollingRollingRolling(corePath)
		if err != nil {
			n.log.Fatal().Err(err).Msgf("core load: %s", corePath)
		}
	}

	retroInit = loadFunction(coreLib, "retro_init")
	retroDeinit = loadFunction(coreLib, "retro_deinit")
	retroAPIVersion = loadFunction(coreLib, "retro_api_version")
	retroGetSystemInfo = loadFunction(coreLib, "retro_get_system_info")
	retroGetSystemAVInfo = loadFunction(coreLib, "retro_get_system_av_info")
	retroSetEnvironment = loadFunction(coreLib, "retro_set_environment")
	retroSetVideoRefresh = loadFunction(coreLib, "retro_set_video_refresh")
	retroSetInputPoll = loadFunction(coreLib, "retro_set_input_poll")
	retroSetInputState = loadFunction(coreLib, "retro_set_input_state")
	retroSetAudioSample = loadFunction(coreLib, "retro_set_audio_sample")
	retroSetAudioSampleBatch = loadFunction(coreLib, "retro_set_audio_sample_batch")
	retroReset = loadFunction(coreLib, "retro_reset")
	retroRun = loadFunction(coreLib, "retro_run")
	retroLoadGame = loadFunction(coreLib, "retro_load_game")
	retroUnloadGame = loadFunction(coreLib, "retro_unload_game")
	retroSerializeSize = loadFunction(coreLib, "retro_serialize_size")
	retroSerialize = loadFunction(coreLib, "retro_serialize")
	retroUnserialize = loadFunction(coreLib, "retro_unserialize")
	retroSetControllerPortDevice = loadFunction(coreLib, "retro_set_controller_port_device")
	retroGetMemorySize = loadFunction(coreLib, "retro_get_memory_size")
	retroGetMemoryData = loadFunction(coreLib, "retro_get_memory_data")

	C.bridge_retro_set_environment(retroSetEnvironment, C.core_environment_cgo)
	C.bridge_retro_set_input_state(retroSetInputState, C.core_input_state_cgo)
	C.bridge_set_callback(retroSetVideoRefresh, C.core_video_refresh_cgo)
	C.bridge_set_callback(retroSetInputPoll, C.core_input_poll_cgo)
	C.bridge_set_callback(retroSetAudioSample, C.core_audio_sample_cgo)
	C.bridge_set_callback(retroSetAudioSampleBatch, C.core_audio_sample_batch_cgo)

	if n.LibCo {
		C.same_thread(retroInit)
	} else {
		C.bridge_call(retroInit)
	}

	n.sys.api = C.bridge_retro_api_version(retroAPIVersion)
	C.bridge_retro_get_system_info(retroGetSystemInfo, &n.sys.i)
	n.log.Info().Msgf("System >>> %v (%v) [%v] nfp: %v, api: %v",
		C.GoString(n.sys.i.library_name), C.GoString(n.sys.i.library_version),
		C.GoString(n.sys.i.valid_extensions), bool(n.sys.i.need_fullpath),
		uint(n.sys.api))
}

func (n *Nanoarch) LoadGame(path string) error {
	game := C.struct_retro_game_info{}

	big := bool(n.sys.i.need_fullpath) // big ROMs are loaded by cores later
	if big {
		size, err := os.StatSize(path)
		if err != nil {
			return err
		}
		game.size = C.size_t(size)
	} else {
		bytes, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// !to pin in 1.21
		ptr := unsafe.Pointer(C.CBytes(bytes))
		game.data = ptr
		game.size = C.size_t(len(bytes))
		defer C.free(ptr)
	}
	fp := C.CString(path)
	defer C.free(unsafe.Pointer(fp))
	game.path = fp

	n.log.Debug().Msgf("ROM - big: %v, size: %v", big, byteCountBinary(int64(game.size)))

	// maybe some custom options
	if n.options4rom != nil {
		romName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if _, ok := n.options4rom[romName]; ok {
			for k, v := range n.options4rom[romName] {
				n.options[k] = v
				n.log.Debug().Msgf("Replace: %v=%v", k, v)
			}
		}
	}

	fmt.Fprintf(stlos.Stderr, "[DIAG LoadGame] checkpoint: before bridge_retro_load_game\n")
	if ok := C.bridge_retro_load_game(retroLoadGame, &game); !ok {
		return fmt.Errorf("core failed to load ROM: %v", path)
	}
	fmt.Fprintf(stlos.Stderr, "[DIAG LoadGame] checkpoint: after bridge_retro_load_game OK\n")

	var av C.struct_retro_system_av_info
	C.bridge_retro_get_system_av_info(retroGetSystemAVInfo, &av)
	n.log.Info().Msgf("System A/V >>> %vx%v (%vx%v), [%vfps], AR [%v], audio [%vHz]",
		av.geometry.base_width, av.geometry.base_height,
		av.geometry.max_width, av.geometry.max_height,
		av.timing.fps, av.geometry.aspect_ratio, av.timing.sample_rate,
	)
	n.log.Info().Msgf("[cloudplay diag] AV info initial base=%dx%d max=%dx%d fps=%.3f ar=%.3f sampleRate=%.1f",
		int(av.geometry.base_width), int(av.geometry.base_height),
		int(av.geometry.max_width), int(av.geometry.max_height),
		float64(av.timing.fps), float64(av.geometry.aspect_ratio), float64(av.timing.sample_rate),
	)
	if isGeometryDifferent(av.geometry) {
		geometryChange(av.geometry)
	}
	n.sys.av = av

	n.serializeSize = 0
	n.log.Debug().Msg("Save file size will be queried on demand")

	Nan0.tickTime = int64(time.Second / time.Duration(n.sys.av.timing.fps))
	if n.vfr {
		n.log.Info().Msgf("variable framerate (VFR) is enabled")
	}

	n.Stopped.Store(false)

	if n.Video.gl.enabled && !n.vulkan.enabled && n.Video.hw == nil {
		n.log.Warn().Msg("GL-capable core did not provide SET_HW_RENDER; falling back to software/non-HW video path")
		fmt.Fprintf(stlos.Stderr, "[DIAG LoadGame] no HW render callback; disabling GL path fallback to software\n")
		n.Video.gl.enabled = false
		n.Video.gl.autoCtx = false
		thread.SwitchGraphics(false)
	}

	fmt.Fprintf(stlos.Stderr, "[DIAG LoadGame] checkpoint: before video init vulkan=%v gl=%v libco=%v\n", n.vulkan.enabled, n.Video.gl.enabled, n.LibCo)
	if n.vulkan.enabled {
		// Vulkan init: headless — no SDL, no thread pinning required.
		// initVulkanVideo creates the context and fires context_reset.
		if n.LibCo {
			C.same_thread(C.init_video_cgo)
		} else {
			initVulkanVideo()
		}
	} else if n.Video.gl.enabled {
		if n.LibCo {
			fmt.Fprintf(stlos.Stderr, "[DIAG LoadGame] checkpoint: before same_thread(init_video_cgo) [LibCo GL]\n")
			C.same_thread(C.init_video_cgo)
			fmt.Fprintf(stlos.Stderr, "[DIAG LoadGame] checkpoint: after same_thread(init_video_cgo), before context_reset [LibCo GL]\n")
			C.same_thread(unsafe.Pointer(Nan0.Video.hw.context_reset))
			fmt.Fprintf(stlos.Stderr, "[DIAG LoadGame] checkpoint: after context_reset [LibCo GL]\n")
		} else {
			runtime.LockOSThread()
			initVideo()
			C.bridge_context_reset(Nan0.Video.hw.context_reset)
			runtime.UnlockOSThread()
		}
	}
	fmt.Fprintf(stlos.Stderr, "[DIAG LoadGame] checkpoint: after video init\n")

	// Default: RETRO_DEVICE_ANALOG on all ports.
	// This exposes dual analog sticks + analog triggers to every core.
	// Cores whose retro_set_controller_port_device is broken for ANALOG
	// (e.g. PCSX2 maps it to pad type "None") get JOYPAD as a workaround.
	defaultDevice := C.unsigned(C.RETRO_DEVICE_ANALOG)
	if needsJoypadWorkaround() {
		defaultDevice = C.unsigned(C.RETRO_DEVICE_JOYPAD)
		n.log.Warn().Msgf("controller device: JOYPAD workaround for core %s (ANALOG broken)", n.meta.LibPath)
	}
	for i := range maxPort {
		C.bridge_retro_set_controller_port_device(retroSetControllerPortDevice, C.uint(i), defaultDevice)
	}

	// map custom devices to ports
	for k, v := range n.meta.Hid {
		for _, device := range v {
			C.bridge_retro_set_controller_port_device(retroSetControllerPortDevice, C.uint(k), C.unsigned(device))
			n.log.Debug().Msgf("set custom port-device: %v:%v", k, device)
		}
	}

	n.LastFrameTime = time.Now().UnixNano()

	fmt.Fprintf(stlos.Stderr, "[DIAG LoadGame] checkpoint: LoadGame returning nil (success)\n")
	return nil
}

func (n *Nanoarch) Shutdown() {
	if n.LibCo {
		thread.Main(func() {
			C.same_thread(retroUnloadGame)
			C.same_thread(retroDeinit)
			if n.vulkan.enabled || n.Video.gl.enabled {
				C.same_thread(C.deinit_video_cgo)
			}
			C.same_thread(C.same_thread_stop)
		})
	} else {
		if n.Video.gl.enabled {
			thread.Main(func() {
				// running inside a go routine, lock the thread to make sure the OpenGL context stays current
				runtime.LockOSThread()
				if err := n.glCtx.BindContext(); err != nil {
					n.log.Error().Err(err).Msg("ctx switch fail")
				}
			})
		}
		C.bridge_call(retroUnloadGame)
		C.bridge_call(retroDeinit)
		if n.vulkan.enabled {
			// Vulkan teardown: no thread pinning needed.
			deinitVulkanVideo()
		} else if n.Video.gl.enabled {
			thread.Main(func() {
				deinitVideo()
				runtime.UnlockOSThread()
			})
		}
	}

	setRotation(0)
	Nan0.sys.av = C.struct_retro_system_av_info{}
	if err := closeLib(coreLib); err != nil {
		n.log.Error().Err(err).Msg("lib close failed")
	}
	n.options = nil
	n.options4rom = nil
	C.free(unsafe.Pointer(n.cUserName))
	C.free(unsafe.Pointer(n.cSaveDirectory))
	C.free(unsafe.Pointer(n.cSystemDirectory))
}

func (n *Nanoarch) Reset() {
	C.bridge_call(retroReset)
}

func (n *Nanoarch) syncInputToCache() {
	n.retropad.SyncToCache()
	if n.keyboardCb != nil {
		n.keyboard.SyncToCache()
	}
	n.mouse.SyncToCache()
}

func (n *Nanoarch) Run() {
	n.syncInputToCache()

	if n.LibCo {
		C.same_thread(retroRun)
	} else {
		// GL requires the OS thread to be locked for the duration of the call
		// so that the OpenGL context stays current.  Vulkan handles are not
		// thread-bound — no locking needed.
		if n.Video.gl.enabled && !n.vulkan.enabled {
			runtime.LockOSThread()
			if err := n.glCtx.BindContext(); err != nil {
				n.log.Error().Err(err).Msg("ctx bind fail")
			}
		}
		C.bridge_call(retroRun)
		if n.Video.gl.enabled && !n.vulkan.enabled {
			runtime.UnlockOSThread()
		}
	}
}

func (n *Nanoarch) IsSupported() error                  { return graphics.TryInit() }
func (n *Nanoarch) IsGL() bool                          { return n.Video.gl.enabled }
func (n *Nanoarch) IsStopped() bool                     { return n.Stopped.Load() }
func (n *Nanoarch) InputRetropad(port int, data []byte) { n.retropad.SetInput(port, data) }
func (n *Nanoarch) InputKeyboard(_ int, data []byte) {
	if n.keyboardCb == nil {
		return
	}

	// we should preserve the state of pressed buttons for the input poll function (each retro_run)
	// and explicitly call the retro_keyboard_callback function when a keyboard event happens
	pressed, key, mod := n.keyboard.SetKey(data)
	C.bridge_retro_keyboard_callback(unsafe.Pointer(n.keyboardCb), C.bool(pressed),
		C.unsigned(key), C.uint32_t(0), C.uint16_t(mod))
}
func (n *Nanoarch) InputMouse(_ int, data []byte) {
	if len(data) == 0 {
		return
	}

	t := data[0]
	state := data[1:]
	switch t {
	case MouseMove:
		n.mouse.ShiftPos(state)
	case MouseButton:
		n.mouse.SetButtons(state[0])
	}
}

func videoSetPixelFormat(format uint32) (C.bool, error) {
	switch format {
	case C.RETRO_PIXEL_FORMAT_0RGB1555:
		Nan0.Video.PixFmt = RGBA5551
		if err := graphics.SetPixelFormat(graphics.UnsignedShort5551); err != nil {
			return false, fmt.Errorf("unknown pixel format %v", Nan0.Video.PixFmt)
		}
	case C.RETRO_PIXEL_FORMAT_XRGB8888:
		Nan0.Video.PixFmt = RGBA8888Rev
		if err := graphics.SetPixelFormat(graphics.UnsignedInt8888Rev); err != nil {
			return false, fmt.Errorf("unknown pixel format %v", Nan0.Video.PixFmt)
		}
	case C.RETRO_PIXEL_FORMAT_RGB565:
		Nan0.Video.PixFmt = RGB565
		if err := graphics.SetPixelFormat(graphics.UnsignedShort565); err != nil {
			return false, fmt.Errorf("unknown pixel format %v", Nan0.Video.PixFmt)
		}
	default:
		return false, fmt.Errorf("unknown pixel type %v", format)
	}
	Nan0.log.Info().Msgf("Pixel format: %v", Nan0.Video.PixFmt)

	return true, nil
}

func setRotation(rot uint) {
	Nan0.Rot = rot
	Nan0.log.Debug().Msgf("Image rotated %v°", rot)
}

func printOpenGLDriverInfo() {
	var openGLInfo strings.Builder
	openGLInfo.Grow(128)
	version, vendor, renderrer, glsl := graphics.GLInfo()
	openGLInfo.WriteString(fmt.Sprintf("\n[OpenGL] Version: %v\n", version))
	openGLInfo.WriteString(fmt.Sprintf("[OpenGL] Vendor: %v\n", vendor))
	openGLInfo.WriteString(fmt.Sprintf("[OpenGL] Renderer: %v\n", renderrer))
	openGLInfo.WriteString(fmt.Sprintf("[OpenGL] GLSL Version: %v", glsl))
	Nan0.log.Debug().Msg(openGLInfo.String())
}

// State defines any memory state of the emulator
type State []byte

type mem struct {
	ptr  unsafe.Pointer
	size uint
}

const (
	CallSerialize   = 1
	CallUnserialize = 2
)

// SaveState returns emulator internal state.
func SaveState() (State, error) {
	size := C.bridge_retro_serialize_size(retroSerializeSize)
	data := make([]byte, uint(size))
	rez := false

	if Nan0.LibCo && !Nan0.hackSkipSameThreadSave {
		rez = *(*bool)(C.same_thread_with_args2(retroSerialize, C.int(CallSerialize), unsafe.Pointer(&data[0]), unsafe.Pointer(&size)))
	} else {
		rez = bool(C.bridge_retro_serialize(retroSerialize, unsafe.Pointer(&data[0]), size))
	}

	if !rez {
		return nil, errors.New("retro_serialize failed")
	}

	return data, nil
}

// RestoreSaveState restores emulator internal state.
func RestoreSaveState(st State) error {
	if len(st) <= 0 {
		return errors.New("empty load state")
	}

	size := C.size_t(len(st))
	rez := false

	if Nan0.LibCo {
		rez = *(*bool)(C.same_thread_with_args2(retroUnserialize, C.int(CallUnserialize), unsafe.Pointer(&st[0]), unsafe.Pointer(&size)))
	} else {
		rez = bool(C.bridge_retro_unserialize(retroUnserialize, unsafe.Pointer(&st[0]), size))
	}

	if !rez {
		return errors.New("retro_unserialize failed")
	}

	return nil
}

// SaveRAM returns the game save RAM (cartridge) data or a nil slice.
func SaveRAM() State {
	memory := ptSaveRAM()
	if memory == nil {
		return nil
	}
	return C.GoBytes(memory.ptr, C.int(memory.size))
}

// RestoreSaveRAM restores game save RAM.
func RestoreSaveRAM(st State) {
	if len(st) > 0 {
		if memory := ptSaveRAM(); memory != nil {
			//noinspection GoRedundantConversion
			copy(unsafe.Slice((*byte)(memory.ptr), memory.size), st)
		}
	}
}

// memorySize returns memory region size.
func memorySize(id C.uint) uint {
	return uint(C.bridge_retro_get_memory_size(retroGetMemorySize, id))
}

// memoryData returns a pointer to memory data.
func memoryData(id C.uint) unsafe.Pointer {
	return C.bridge_retro_get_memory_data(retroGetMemoryData, id)
}

// ptSaveRam return SRAM memory pointer if core supports it or nil.
func ptSaveRAM() *mem {
	ptr, size := memoryData(C.RETRO_MEMORY_SAVE_RAM), memorySize(C.RETRO_MEMORY_SAVE_RAM)
	if ptr == nil || size == 0 {
		return nil
	}
	return &mem{ptr: ptr, size: size}
}

func byteCountBinary(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func (m Metadata) HasHack(h string) bool {
	for _, n := range m.Hacks {
		if h == n {
			return true
		}
	}
	return false
}

var diagVideoRefreshCount int64

var (
	retroAPIVersion              unsafe.Pointer
	retroDeinit                  unsafe.Pointer
	retroGetSystemAVInfo         unsafe.Pointer
	retroGetSystemInfo           unsafe.Pointer
	coreLib                      unsafe.Pointer
	retroInit                    unsafe.Pointer
	retroLoadGame                unsafe.Pointer
	retroReset                   unsafe.Pointer
	retroRun                     unsafe.Pointer
	retroSetAudioSample          unsafe.Pointer
	retroSetAudioSampleBatch     unsafe.Pointer
	retroSetControllerPortDevice unsafe.Pointer
	retroSetEnvironment          unsafe.Pointer
	retroSetInputPoll            unsafe.Pointer
	retroSetInputState           unsafe.Pointer
	retroSetVideoRefresh         unsafe.Pointer
	retroUnloadGame              unsafe.Pointer
	retroGetMemoryData           unsafe.Pointer
	retroGetMemorySize           unsafe.Pointer
	retroSerialize               unsafe.Pointer
	retroSerializeSize           unsafe.Pointer
	retroUnserialize             unsafe.Pointer
)

//export coreVideoRefresh
func coreVideoRefresh(data unsafe.Pointer, width, height uint, packed uint) {
	// When Stopped, Vulkan cores (LRPS2) still need their frame cycle to
	// complete so retro_run yields back.  Accept the frame through the
	// Vulkan readback path (which triggers go_set_image / lock / unlock
	// callbacks) but skip the output to the media pipeline.
	if Nan0.Stopped.Load() {
		if Nan0.vulkan.enabled && data == C.RETRO_HW_FRAME_BUFFER_VALID {
			readVulkanFramebuffer(0, width, height)
		}
		return
	}

	// DIAG: unconditional frame counter to verify coreVideoRefresh is called
	diagN := atomic.AddInt64(&diagVideoRefreshCount, 1)
	if diagN <= 12 || diagN%120 == 0 {
		fmt.Fprintf(stlos.Stderr, "[DIAG coreVideoRefresh] ts=%d frame=%d w=%d h=%d packed=%d bpp=%d data_nil=%v vulkan=%v hw_valid=%v\n",
			time.Now().UnixNano(), diagN, width, height, packed, Nan0.Video.PixFmt.BPP, data == nil, Nan0.vulkan.enabled, data == C.RETRO_HW_FRAME_BUFFER_VALID)
	}

	// Some cores can render slower or faster than the internal 1/fps core tick,
	// so historically we tracked actual render time for RTP timestamps.
	//
	// For the current Vulkan/GameCube path this makes cadence visibly worse: the
	// stream becomes hitchy because the sender duration follows wall-clock jitter
	// instead of a predictable display cadence. Prioritize stable pacing here.
	dt := Nan0.tickTime
	if Nan0.vfr && !Nan0.vulkan.enabled {
		t := time.Now().UnixNano()
		dt = t - Nan0.LastFrameTime
		Nan0.LastFrameTime = t
	}

	// when the core returns a duplicate frame
	if data == nil {
		Nan0.Handlers.OnDup()
		return
	}

	// calculate real frame width in pixels from packed data (realWidth >= width)
	// some cores or games output zero pitch, i.e. N64 Mupen
	bpp := Nan0.Video.PixFmt.BPP
	if packed == 0 {
		packed = width * bpp
	}
	// calculate space for the video frame
	bytes := packed * height

	var data_ []byte
	if data != C.RETRO_HW_FRAME_BUFFER_VALID {
		//noinspection GoRedundantConversion
		data_ = unsafe.Slice((*byte)(data), bytes)
		// DIAG: check if data pointer actually has non-zero content
		if diagN <= 3 {
			nonZero := 0
			for i := 0; i < len(data_) && i < 2000; i++ {
				if data_[i] != 0 { nonZero++; if nonZero > 5 { break } }
			}
			fmt.Fprintf(stlos.Stderr, "[DIAG coreVideoRefresh] frame=%d ptr=%p bytes=%d first16=%v nonZeroIn2k=%d\n",
				diagN, data, bytes, data_[:min(16, len(data_))], nonZero)
		}
	} else if Nan0.vulkan.enabled {
		// Vulkan HW render: read back from Vulkan staging buffer.
		data_ = readVulkanFramebuffer(bytes, width, height)
	} else {
		// GL HW render: capture callback-time framebuffer bindings before the
		// core or driver resets them, then read back.
		drawFbo, readFbo := graphics.CaptureHwFramebufferBindings()
		if diagN <= 12 || diagN%120 == 0 {
			fmt.Fprintf(stlos.Stderr, "[DIAG coreVideoRefresh GL bindings] frame=%d drawFbo=%d readFbo=%d\n", diagN, drawFbo, readFbo)
		}
		data_ = graphics.ReadFramebuffer(bytes, width, height)
		// OpenGL uses bottom-left origin; flip for GL-fallback cores.
		if Nan0.Video.hw != nil && bool(Nan0.Video.hw.bottom_left_origin) {
			data_ = flipFrameVertical(data_, int(width), int(height), int(Nan0.Video.PixFmt.BPP))
		}
	}

	// some cores or games have a variable output frame size, i.e. PSX Rearmed
	// also we have an option of xN output frame magnification
	// so, it may be rescaled
	//
	// Dynamic resolution fix: some cores (notably Angrylion for N64) change
	// their output resolution mid-game without calling SET_GEOMETRY or
	// SET_SYSTEM_AV_INFO.  Detect this by comparing the actual frame
	// dimensions against the current base geometry and synthesize a geometry
	// change so the downstream encoder reinitialises at the correct size.
	if C.unsigned(width) != Nan0.sys.av.geometry.base_width || C.unsigned(height) != Nan0.sys.av.geometry.base_height {
		geom := Nan0.sys.av.geometry
		geom.base_width = C.unsigned(width)
		geom.base_height = C.unsigned(height)
		geometryChange(geom)
	}

	Nan0.Handlers.OnVideo(data_, int32(dt), FrameInfo{W: width, H: height, Stride: packed})
}

//export coreRumble
func coreRumble(port C.unsigned, effect C.unsigned, strength C.uint16_t) C.bool {
	if Nan0.Stopped.Load() {
		return false
	}
	Nan0.Handlers.OnRumble(uint(port), uint(effect), uint16(strength))
	return true
}

//export coreAudioSampleBatch
func coreAudioSampleBatch(data unsafe.Pointer, frames C.size_t) C.size_t {
	if Nan0.Stopped.Load() {
		return frames
	}
	Nan0.Handlers.OnAudio(data, int(frames)<<1)
	return frames
}

func m(m *C.char) string { return strings.TrimRight(C.GoString(m), "\n") }

//export coreLog
func coreLog(level C.enum_retro_log_level, msg *C.char) {
	switch level {
	// with debug level cores have too much logs
	case C.RETRO_LOG_DEBUG:
		Nan0.log.Debug().MsgFunc(func() string { return m(msg) })
	case C.RETRO_LOG_INFO:
		Nan0.log.Info().MsgFunc(func() string { return m(msg) })
	case C.RETRO_LOG_WARN:
		Nan0.log.Warn().MsgFunc(func() string { return m(msg) })
	case C.RETRO_LOG_ERROR:
		Nan0.log.Error().MsgFunc(func() string { return m(msg) })
	default:
		Nan0.log.Log().MsgFunc(func() string { return m(msg) })
		// RETRO_LOG_DUMMY = INT_MAX
	}
}

var fboDiagN int64

//export coreGetCurrentFramebuffer
func coreGetCurrentFramebuffer() C.uintptr_t {
	fboId := graphics.GlFbo()
	n := atomic.AddInt64(&fboDiagN, 1)
	if n <= 5 || n%600 == 0 {
		fmt.Fprintf(stlos.Stderr, "[DIAG coreGetCurrentFramebuffer] n=%d fbo=%d\n", n, fboId)
	}
	return (C.uintptr_t)(fboId)
}

//export coreGetProcAddress
func coreGetProcAddress(sym *C.char) C.retro_proc_address_t {
	return (C.retro_proc_address_t)(graphics.GlProcAddress(C.GoString(sym)))
}

//export coreEnvironment
func coreEnvironment(cmd C.unsigned, data unsafe.Pointer) C.bool {

	// see core_environment_cgo

	switch cmd {
	case C.RETRO_ENVIRONMENT_SET_SYSTEM_AV_INFO:
		Nan0.log.Debug().Msgf("retro_set_system_av_info")
		av := *(*C.struct_retro_system_av_info)(data)
		if isGeometryDifferent(av.geometry) {
			geometryChange(av.geometry)
		}
		return true
	case C.RETRO_ENVIRONMENT_SET_GEOMETRY:
		Nan0.log.Debug().Msgf("retro_set_geometry")
		geom := *(*C.struct_retro_game_geometry)(data)
		if isGeometryDifferent(geom) {
			geometryChange(geom)
		}
		return true
	case C.RETRO_ENVIRONMENT_SET_ROTATION:
		setRotation((*(*uint)(data) % 4) * 90)
		return true
	case C.RETRO_ENVIRONMENT_GET_CAN_DUPE:
		dup := C.bool(Nan0.meta.FrameDup)
		*(*C.bool)(data) = dup
		return dup
	case C.RETRO_ENVIRONMENT_GET_USERNAME:
		*(**C.char)(data) = Nan0.cUserName
		return true
	case C.RETRO_ENVIRONMENT_GET_LOG_INTERFACE:
		cb := (*C.struct_retro_log_callback)(data)
		cb.log = (C.retro_log_printf_t)(C.core_log_cgo)
		return true
	case C.RETRO_ENVIRONMENT_SET_PIXEL_FORMAT:
		res, err := videoSetPixelFormat(*(*C.enum_retro_pixel_format)(data))
		if err != nil {
			Nan0.log.Fatal().Err(err).Msg("pix format failed")
		}
		return res
	case C.RETRO_ENVIRONMENT_GET_SYSTEM_DIRECTORY:
		*(**C.char)(data) = Nan0.cSystemDirectory
		return true
	case C.RETRO_ENVIRONMENT_GET_SAVE_DIRECTORY:
		*(**C.char)(data) = Nan0.cSaveDirectory
		return true
	case C.RETRO_ENVIRONMENT_SET_MESSAGE:
		// only with the Libretro debug mode
		if Nan0.log.GetLevel() < logger.InfoLevel {
			message := (*C.struct_retro_message)(data)
			msg := C.GoString(message.msg)
			Nan0.log.Debug().Msgf("message: %v", msg)
			return true
		}
		return false
	case C.RETRO_ENVIRONMENT_GET_VARIABLE:
		if Nan0.options == nil {
			return false
		}
		rv := (*C.struct_retro_variable)(data)
		key := C.GoString(rv.key)
		if v, ok := Nan0.options[key]; ok {
			// make Go strings null-terminated copies ;_;
			Nan0.options[key] = v + "\x00"
			ptr := unsafe.Pointer(unsafe.StringData(Nan0.options[key]))
			var p runtime.Pinner
			p.Pin(ptr)
			defer p.Unpin()
			// cast to C string and set the value
			rv.value = (*C.char)(ptr)
			Nan0.log.Debug().Msgf("Set %v=%v", key, v)
			return true
		}
		return false
	case C.RETRO_ENVIRONMENT_GET_PREFERRED_HW_RENDER:
		if preferredHWContextIsVulkan() {
			*(*C.enum_retro_hw_context_type)(data) = C.RETRO_HW_CONTEXT_VULKAN
			Nan0.log.Debug().Msg("Advertising Vulkan as preferred HW render context")
			return true
		}
		return false
	case C.RETRO_ENVIRONMENT_SET_HW_RENDER:
		hw := (*C.struct_retro_hw_render_callback)(data)
		if hw.context_type == C.RETRO_HW_CONTEXT_VULKAN {
			// Delegate to Vulkan wiring (nanoarch_vulkan.go).
			// On !vulkan builds handleVulkanHWRender returns false.
			if handleVulkanHWRender(data) {
				return true
			}
			return false
		}
		if Nan0.Video.gl.enabled {
			Nan0.Video.hw = hw
			Nan0.Video.hw.get_current_framebuffer = (C.retro_hw_get_current_framebuffer_t)(C.core_get_current_framebuffer_cgo)
			Nan0.Video.hw.get_proc_address = (C.retro_hw_get_proc_address_t)(C.core_get_proc_address_cgo)
			return true
		}
		return false
	case C.RETRO_ENVIRONMENT_GET_HW_RENDER_INTERFACE:
		if handleGetHWRenderInterface(data) {
			return true
		}
		return false
	case C.RETRO_ENVIRONMENT_SET_CONTROLLER_INFO:
		if Nan0.log.GetLevel() > logger.DebugLevel {
			return false
		}

		info := (*[64]C.struct_retro_controller_info)(data)
		for c, controller := range info {
			tp := unsafe.Pointer(controller.types)
			if tp == nil {
				break
			}
			cInfo := strings.Builder{}
			cInfo.WriteString(fmt.Sprintf("Controller [%v] ", c))
			cd := (*[32]C.struct_retro_controller_description)(tp)
			delim := ", "
			n := int(controller.num_types)
			for i := range n {
				if i == n-1 {
					delim = ""
				}
				cInfo.WriteString(fmt.Sprintf("%v: %v%s", cd[i].id, C.GoString(cd[i].desc), delim))
			}
			//Nan0.log.Debug().Msgf("%v", cInfo.String())
		}
		return true
	case C.RETRO_ENVIRONMENT_SET_KEYBOARD_CALLBACK:
		Nan0.log.Debug().Msgf("Keyboard event callback was set")
		Nan0.keyboardCb = (*C.struct_retro_keyboard_callback)(data)
		return true
	case C.RETRO_ENVIRONMENT_SET_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE:
		// The core provides a retro_hw_render_context_negotiation_interface_vulkan
		// so it can participate in device creation (e.g. add required extensions).
		// Delegate to Vulkan-specific handling.
		return C.bool(handleSetHWRenderContextNegotiation(data))
	case C.RETRO_ENVIRONMENT_GET_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_SUPPORT:
		// Indicate to the core which negotiation interface versions we support.
		return C.bool(handleGetHWRenderContextNegotiationSupport(data))
	case C.RETRO_ENVIRONMENT_GET_RUMBLE_INTERFACE:
		rumble := (*C.struct_retro_rumble_interface)(data)
		rumble.set_rumble_state = (C.retro_set_rumble_state_t)(C.core_rumble_cgo)
		Nan0.log.Info().Msg("Rumble interface registered")
		return true
	}
	if cmd != 15 && cmd != 65576 && cmd != 10 && cmd != 65546 {
		Nan0.log.Debug().Msgf("Unhandled env cmd=%d", cmd)
	}
	return false
}

//export initVideo
func initVideo() {
	// Vulkan path: create headless context, skip SDL/GL entirely.
	if Nan0.vulkan.enabled {
		initVulkanVideo()
		return
	}
	if Nan0.Video.hw == nil {
		Nan0.log.Warn().Msg("initVideo called without HW render callback; skipping GL context init")
		return
	}

	var context graphics.Context
	switch Nan0.Video.hw.context_type {
	case C.RETRO_HW_CONTEXT_NONE:
		context = graphics.CtxNone
	case C.RETRO_HW_CONTEXT_OPENGL:
		context = graphics.CtxOpenGl
	case C.RETRO_HW_CONTEXT_OPENGLES2:
		context = graphics.CtxOpenGlEs2
	case C.RETRO_HW_CONTEXT_OPENGL_CORE:
		context = graphics.CtxOpenGlCore
	case C.RETRO_HW_CONTEXT_OPENGLES3:
		context = graphics.CtxOpenGlEs3
	case C.RETRO_HW_CONTEXT_OPENGLES_VERSION:
		context = graphics.CtxOpenGlEsVersion
	case C.RETRO_HW_CONTEXT_VULKAN:
		context = graphics.CtxVulkan
	case C.RETRO_HW_CONTEXT_DUMMY:
		context = graphics.CtxDummy
	default:
		context = graphics.CtxUnknown
	}

	thread.Main(func() {
		var err error
		Nan0.glCtx, err = graphics.NewHeadlessGLContext(graphics.Config{
			Ctx:            context,
			W:              int(Nan0.sys.av.geometry.max_width),
			H:              int(Nan0.sys.av.geometry.max_height),
			GLAutoContext:  Nan0.Video.gl.autoCtx,
			GLVersionMajor: uint(Nan0.Video.hw.version_major),
			GLVersionMinor: uint(Nan0.Video.hw.version_minor),
			GLHasDepth:     bool(Nan0.Video.hw.depth),
			GLHasStencil:   bool(Nan0.Video.hw.stencil),
		})
		if err != nil {
			panic(err)
		}
	})

	if Nan0.log.GetLevel() < logger.InfoLevel {
		printOpenGLDriverInfo()
	}
}

//export deinitVideo
func deinitVideo() {
	// Vulkan path: tear down headless context, skip SDL.
	if Nan0.vulkan.enabled {
		deinitVulkanVideo()
		Nan0.hackSkipSameThreadSave = false
		return
	}

	if !Nan0.hackSkipHwContextDestroy && Nan0.Video.hw != nil {
		C.bridge_context_reset(Nan0.Video.hw.context_destroy)
	}
	thread.Main(func() {
		if err := Nan0.glCtx.Deinit(); err != nil {
			Nan0.log.Error().Err(err).Msg("deinit fail")
		}
	})
	Nan0.Video.gl.enabled = false
	Nan0.Video.gl.autoCtx = false
	Nan0.hackSkipHwContextDestroy = false
	Nan0.hackSkipSameThreadSave = false
	thread.SwitchGraphics(false)
}

type limit struct {
	d  time.Duration
	t  *time.Timer
	mu sync.Mutex
}

func NewLimit(d time.Duration) func(f func()) {
	l := &limit{d: d}
	return func(f func()) { l.push(f) }
}

func (d *limit) push(f func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.t != nil {
		d.t.Stop()
	}
	d.t = time.AfterFunc(d.d, f)
}

func geometryChange(geom C.struct_retro_game_geometry) {
	Nan0.limiter(func() {
		old := Nan0.sys.av.geometry
		Nan0.sys.av.geometry = geom

		if Nan0.Video.gl.enabled && (old.max_width != geom.max_width || old.max_height != geom.max_height) {
			// (for LRPS2) makes the max height bigger increasing SDL2 and OpenGL buffers slightly
			Nan0.sys.av.geometry.max_height = C.unsigned(float32(Nan0.sys.av.geometry.max_height) * 1.5)
			bufS := uint(geom.max_width*Nan0.sys.av.geometry.max_height) * Nan0.Video.PixFmt.BPP
			graphics.SetBuffer(int(bufS))
			Nan0.log.Debug().Msgf("OpenGL frame buffer: %v", bufS)
		}

		Nan0.log.Info().Msgf("[cloudplay diag] geometryChange oldBase=%dx%d oldMax=%dx%d newBase=%dx%d newMax=%dx%d ar=%.3f gl=%v",
			int(old.base_width), int(old.base_height), int(old.max_width), int(old.max_height),
			int(geom.base_width), int(geom.base_height), int(geom.max_width), int(geom.max_height),
			float64(geom.aspect_ratio), Nan0.Video.gl.enabled,
		)
		if Nan0.OnSystemAvInfo != nil {
			Nan0.log.Debug().Msgf(">>> geometry change %v -> %v", old, geom)
			go Nan0.OnSystemAvInfo()
		}
	})
}

func isGeometryDifferent(geom C.struct_retro_game_geometry) bool {
	return Nan0.sys.av.geometry.base_width != geom.base_width ||
		Nan0.sys.av.geometry.base_height != geom.base_height
}

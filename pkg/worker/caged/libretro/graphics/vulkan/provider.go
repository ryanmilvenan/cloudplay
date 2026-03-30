//go:build vulkan

package vulkan

/*
#cgo LDFLAGS: -lvulkan
#include "libretro_vulkan.h"
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// ── C-callable shim functions that proxy back into Go ──────────────────────
// These have the exact signatures required by the libretro Vulkan interface v5
// and are registered as function pointers in the render interface struct.
//
// Phase 3 fix: wait_sync_index takes only (handle) — no index parameter.
// The old signature go_wait_sync_index(handle, index) was wrong per the spec.

extern void   go_set_image(void *handle, struct retro_vulkan_image *image,
                           uint32_t num_sems, VkSemaphore *sems, uint32_t src_qf);
extern uint32_t go_get_sync_index(void *handle);
extern uint32_t go_get_sync_index_mask(void *handle);
extern void   go_set_command_buffers(void *handle, uint32_t num, VkCommandBuffer *cmds);
extern void   go_wait_sync_index(void *handle);
extern void   go_lock_queue(void *handle);
extern void   go_unlock_queue(void *handle);
extern void   go_set_signal_semaphore(void *handle, VkSemaphore sem);

static void bridge_set_image(
    void *handle,
    const struct retro_vulkan_image *image,
    uint32_t num_sems,
    const VkSemaphore *sems,
    uint32_t src_qf)
{
    go_set_image(handle, (struct retro_vulkan_image *)image, num_sems, (VkSemaphore *)sems, src_qf);
}

static void bridge_set_command_buffers(void *handle, uint32_t num, const VkCommandBuffer *cmds) {
    go_set_command_buffers(handle, num, (VkCommandBuffer *)cmds);
}

// Minimal dummy negotiation interface.
// Populated with interface_type=0 (Vulkan), interface_version=0 so the core
// knows we don't implement any useful negotiation callbacks. All callbacks are
// NULL so well-behaved cores NULL-check before calling.
// This is provided so iface->negotiation_interface is non-NULL and Dolphin's
// context_reset doesn't crash dereferencing a NULL negotiation pointer.
static struct retro_hw_render_context_negotiation_interface_vulkan g_dummy_neg = {
    0,    // interface_type  = RETRO_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_VULKAN (0)
    0,    // interface_version = 0 (no callbacks implemented)
    NULL, // get_application_info
    NULL, // create_device
    NULL, // destroy_device
    NULL, // create_instance (v2)
    NULL, // create_device2  (v2)
};

// Build the runtime interface struct in caller-owned C memory, wiring in the
// shim callbacks.
//
// Phase 3 fix: interface_version = RETRO_HW_RENDER_INTERFACE_VULKAN_VERSION (5).
// Populate get_device_proc_addr and get_instance_proc_addr so Dolphin can
// resolve additional Vulkan entry points without linking against libvulkan.
static void init_vulkan_interface(
    struct retro_hw_render_interface_vulkan *iface,
    uintptr_t      handle,
    VkInstance     instance,
    VkPhysicalDevice gpu,
    VkDevice       device,
    VkQueue        queue,
    unsigned       queue_index)
{
    memset(iface, 0, sizeof(*iface));
    iface->interface_type    = RETRO_HW_RENDER_INTERFACE_VULKAN;
    iface->interface_version = RETRO_HW_RENDER_INTERFACE_VULKAN_VERSION; // 5
    iface->handle            = (void *)handle;
    iface->instance          = instance;
    iface->gpu               = gpu;
    iface->device            = device;

    // v5: proc-addr loaders must come before queue in the struct.
    // Use the standard Vulkan loader entry points.
    iface->get_device_proc_addr   = vkGetDeviceProcAddr;
    iface->get_instance_proc_addr = vkGetInstanceProcAddr;

    iface->queue             = queue;
    iface->queue_index       = queue_index;

    iface->set_image            = bridge_set_image;
    iface->get_sync_index       = go_get_sync_index;
    iface->get_sync_index_mask  = go_get_sync_index_mask;
    iface->set_command_buffers  = bridge_set_command_buffers;
    iface->wait_sync_index      = go_wait_sync_index;
    iface->lock_queue           = go_lock_queue;
    iface->unlock_queue         = go_unlock_queue;
    iface->set_signal_semaphore = go_set_signal_semaphore;

    // Provide a non-NULL negotiation_interface so Dolphin's context_reset
    // doesn't dereference a NULL pointer when it checks negotiation fields.
    // All callbacks are NULL so cores NULL-check before calling.
    iface->negotiation_interface = &g_dummy_neg;
}
*/
import "C"
import (
	"fmt"
	"log"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// ── Frame timing instrumentation ──────────────────────────────────────────
// Lightweight per-frame timing emitted every 300 frames (~5 s at 60 fps).
// Set CLOUDPLAY_FRAME_TIMING=1 env var at runtime to enable.
// This is a compile-time zero-cost path unless the env var is set.

var (
	frameTimingEnabled int32 // set to 1 by init() if env var present
	frameCount         int64
)

func init() {
	// Check env var without importing os at package init — use syscall-free
	// approach: read the global that nanoarch sets when CLOUDPLAY_FRAME_TIMING=1.
	// For simplicity we check via a package-level var that nanoarch can set.
}

// EnableFrameTiming turns on per-frame timing logs.  Call from nanoarch init
// when CLOUDPLAY_FRAME_TIMING=1 is detected.
func EnableFrameTiming() { atomic.StoreInt32(&frameTimingEnabled, 1) }

// FrameTimer accumulates timing for a single frame's phases.
type FrameTimer struct {
	start   time.Time
	waitEnd time.Time
	blitEnd time.Time
	readEnd time.Time
}

func newFrameTimer() *FrameTimer {
	if atomic.LoadInt32(&frameTimingEnabled) == 0 {
		return nil
	}
	return &FrameTimer{start: time.Now()}
}

func (ft *FrameTimer) markWaitDone()  { ft.waitEnd = time.Now() }
func (ft *FrameTimer) markBlitDone()  { ft.blitEnd = time.Now() }
func (ft *FrameTimer) markReadDone()  { ft.readEnd = time.Now() }

func (ft *FrameTimer) emit(isZeroCopy bool) {
	n := atomic.AddInt64(&frameCount, 1)
	if n%300 != 0 {
		return
	}
	total := ft.readEnd.Sub(ft.start)
	blit := ft.blitEnd.Sub(ft.waitEnd)
	read := ft.readEnd.Sub(ft.blitEnd)
	path := "cpu-readback"
	if isZeroCopy {
		path = "zero-copy-gpu"
	}
	log.Printf("[cloudplay/frame-timing] frame=%d path=%s total=%s blit=%s read=%s",
		n, path, total.Round(time.Microsecond), blit.Round(time.Microsecond), read.Round(time.Microsecond))
}

// ── Interface version constants (match libretro_vulkan.h) ─────────────────

const (
	RetroHWRenderInterfaceVulkan                   = 0
	RetroHWRenderInterfaceVulkanVersion            = 5 // Phase 3 fix: was incorrectly 1
	RetroHWRenderContextNegotiationInterfaceVulkan = 0
	NegotiationInterfaceVulkanVersion              = 2
)

// ── Provider ──────────────────────────────────────────────────────────────

// Provider manages the libretro Vulkan HW render interface for a single core.
// It holds the Vulkan context and the most-recently-provided rendered image.
type Provider struct {
	mu  sync.Mutex
	ctx *Context

	// currentImage is the VkImage most recently provided by the core via set_image.
	// May be nil if the core only provided an image view (standard path);
	// in that case ReadFrame will use a fallback strategy.
	currentImage     C.VkImage
	currentLayout    C.VkImageLayout
	currentImageView C.VkImageView
	// syncIndex cycles between 0 and 1 (double buffer).
	syncIndex uint32

	// frameCapture is created on-demand when the first image arrives (Phase 2).
	fc *FrameCapture

	// zeroCopy is the Phase 3 exportable device-local buffer.
	// Non-nil only when ctx.ExternalMemoryEnabled is true and the buffer has
	// been successfully allocated.  ReadFrameZeroCopy uses this path.
	zeroCopy *ZeroCopyBuffer

	// zeroCopyBlitDone tracks whether BlitFrom has already been called for
	// the current frame image.  This prevents the double-blit that occurs
	// when ReadFrame (called from readVulkanFramebuffer) does a blit as a
	// side-effect and then ReadFrameZeroCopy (called from the media pipeline)
	// does a second blit for the same image.
	// Reset to false each time go_set_image records a new image from the core.
	zeroCopyBlitDone bool

	// Interface struct we hand to the core via GET_HW_RENDER_INTERFACE.
	// Stored in C-owned memory because the libretro core retains this pointer.
	iface  *C.struct_retro_hw_render_interface_vulkan
	handle cgo.Handle

	// queueMu guards lock/unlock_queue calls.
	queueMu sync.Mutex
}

func registerProvider(p *Provider) C.uintptr_t {
	p.handle = cgo.NewHandle(p)
	return C.uintptr_t(p.handle)
}

func lookupProvider(handle unsafe.Pointer) *Provider {
	if handle == nil {
		return nil
	}
	return cgo.Handle(uintptr(handle)).Value().(*Provider)
}

func unregisterProvider(p *Provider) {
	if p.handle != 0 {
		p.handle.Delete()
		p.handle = 0
	}
}

// NewProvider creates a Provider that wraps an existing Vulkan context.
//
// If ctx.ExternalMemoryEnabled is true the provider will allocate a Phase 3
// ZeroCopyBuffer at first use (deferred until the render dimensions are known
// in ReadFrame).  The Phase 2 staging-buffer path is kept as fallback.
func NewProvider(ctx *Context) (*Provider, error) {
	p := &Provider{
		ctx:           ctx,
		currentLayout: C.VK_IMAGE_LAYOUT_UNDEFINED,
	}

	handle := registerProvider(p)
	p.iface = (*C.struct_retro_hw_render_interface_vulkan)(C.calloc(1, C.size_t(C.sizeof_struct_retro_hw_render_interface_vulkan)))
	if p.iface == nil {
		unregisterProvider(p)
		return nil, fmt.Errorf("vulkan: failed to allocate interface struct")
	}

	// Build the interface struct that the core will query via
	// RETRO_ENVIRONMENT_GET_HW_RENDER_INTERFACE.
	C.init_vulkan_interface(
		p.iface,
		handle,
		ctx.Instance,
		ctx.PhysDevice,
		ctx.Device,
		ctx.Queue,
		C.unsigned(ctx.QueueFamily),
	)

	return p, nil
}

// zeroCopyAvailable reports whether the Phase 3 path can be used.
func (p *Provider) zeroCopyAvailable() bool {
	return p.ctx.ExternalMemoryEnabled
}

// ensureZeroCopy allocates (or re-allocates) the ZeroCopyBuffer when the
// render dimensions are known.  It is called lazily from ReadFrameZeroCopy.
func (p *Provider) ensureZeroCopy(w, h uint32) error {
	if p.zeroCopy != nil && p.zeroCopy.width == w && p.zeroCopy.height == h {
		return nil // already allocated for this size
	}
	if p.zeroCopy != nil {
		p.zeroCopy.Destroy()
		p.zeroCopy = nil
	}
	zc, err := NewZeroCopyBuffer(p.ctx, w, h)
	if err != nil {
		return err
	}
	p.zeroCopy = zc
	return nil
}

// ZeroCopyBuffer returns the underlying ZeroCopyBuffer if Phase 3 is active,
// or nil otherwise.  Used by nanoarch to wire the fd into the CUDA encoder.
func (p *Provider) ZeroCopyBuffer() *ZeroCopyBuffer {
	return p.zeroCopy
}

// Interface returns a pointer to the retro_hw_render_interface_vulkan struct.
// Pass this pointer to the core in response to GET_HW_RENDER_INTERFACE.
func (p *Provider) Interface() *C.struct_retro_hw_render_interface_vulkan {
	return p.iface
}

// QueueLock acquires the shared queue mutex, preventing concurrent access
// to the Vulkan queue by both the frontend's readback code and the core's
// lock_queue/unlock_queue callbacks.
func (p *Provider) QueueLock() { p.queueMu.Lock() }

// QueueUnlock releases the shared queue mutex.
func (p *Provider) QueueUnlock() { p.queueMu.Unlock() }

// CurrentImage returns the most recently set VkImage and its layout, if any.
func (p *Provider) CurrentImage() (img C.VkImage, layout C.VkImageLayout, view C.VkImageView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentImage, p.currentLayout, p.currentImageView
}

// ReadFrame copies the current rendered frame to CPU memory and returns RGBA bytes.
// w and h must match the actual rendered dimensions.
//
// Phase routing:
//   - If Phase 3 (zero-copy) is available, ReadFrameZeroCopy is called instead and
//     the blitted GPU buffer fd is made available for CUDA import; ReadFrame itself
//     still returns a CPU copy for callers that need []byte (e.g. software path).
//     In a fully wired Phase 3 deployment the caller should skip ReadFrame entirely
//     and drive encoding via ZeroCopyBuffer.ExportMemoryFd().
//   - Otherwise falls through to the Phase 2 staging-buffer readback.
func (p *Provider) ReadFrame(w, h uint32) ([]byte, error) {
	ft := newFrameTimer()

	p.mu.Lock()
	img := p.currentImage
	layout := p.currentLayout
	p.mu.Unlock()

	if ft != nil {
		ft.markWaitDone()
	}

	if img == nil {
		// No image yet — return a blank frame.
		size := int(w * h * 4)
		return make([]byte, size), nil
	}

	// Phase 3 path: blit into exportable device-local buffer.
	// We do NOT blit here if the media pipeline will call ReadFrameZeroCopy
	// separately (it checks zeroCopyBlitDone to avoid double-blit).
	// This side-effect blit is only done when zero-copy is available so the
	// fd is ready by the time the media pipeline asks for it.
	zeroCopyActive := false
	if p.zeroCopyAvailable() {
		p.mu.Lock()
		alreadyBlitted := p.zeroCopyBlitDone
		p.mu.Unlock()
		if !alreadyBlitted {
			if err := p.ensureZeroCopy(w, h); err == nil {
				if blitErr := p.zeroCopy.BlitFrom(img, layout); blitErr == nil {
					zeroCopyActive = true
					p.mu.Lock()
					p.zeroCopyBlitDone = true
					p.mu.Unlock()
				}
			}
		} else {
			zeroCopyActive = true // blit already done this frame
		}
	}

	if ft != nil {
		ft.markBlitDone()
	}

	// Phase 2 path: CPU readback via staging buffer (kept as fallback / for
	// callers that still need []byte pixel data).
	if p.fc == nil || p.fc.width != w || p.fc.height != h {
		if p.fc != nil {
			p.fc.Destroy()
		}
		var err error
		p.fc, err = NewFrameCapture(p.ctx, w, h)
		if err != nil {
			return nil, err
		}
	}

	pixels, err := p.fc.Readback(img, layout)
	if ft != nil {
		ft.markReadDone()
		ft.emit(zeroCopyActive)
	}
	return pixels, err
}

// ReadFrameZeroCopy performs the Phase 3 GPU-to-GPU blit into the exportable
// buffer and returns the buffer's fd for CUDA import.  It does NOT copy pixels
// to the CPU.
//
// Returns (-1, err) if Phase 3 is unavailable or the blit fails.
//
// Typical call flow (nanoarch_vulkan.go, Phase 3 fully wired):
//
//	fd, err := provider.ReadFrameZeroCopy(w, h)
//	// → CUDA: cuMemImportFromShareableHandle(fd)
//	// → NVENC: nvenc.EncodeFromDevPtr(cudaPtr, size)
func (p *Provider) ReadFrameZeroCopy(w, h uint32) (fd int, err error) {
	if !p.zeroCopyAvailable() {
		return -1, fmt.Errorf("zerocopy: not available on this device")
	}

	p.mu.Lock()
	img := p.currentImage
	layout := p.currentLayout
	p.mu.Unlock()

	if img == nil {
		return -1, fmt.Errorf("zerocopy: no current image")
	}

	if err = p.ensureZeroCopy(w, h); err != nil {
		return -1, err
	}

	// Avoid double-blit: ReadFrame may have already blitted this frame as a
	// side effect (when zero-copy is available).  Check zeroCopyBlitDone.
	p.mu.Lock()
	alreadyBlitted := p.zeroCopyBlitDone
	p.mu.Unlock()

	if !alreadyBlitted {
		if err = p.zeroCopy.BlitFrom(img, layout); err != nil {
			return -1, err
		}
		p.mu.Lock()
		p.zeroCopyBlitDone = true
		p.mu.Unlock()
	}

	fd, err = p.zeroCopy.ExportMemoryFd()
	if err == nil {
		// Confirm zero-copy hot-path is active every 300 frames.
		n := atomic.AddInt64(&frameCount, 1)
		if n%300 == 0 {
			log.Printf("[cloudplay/zero-copy] frame=%d fd=%d active=true (GPU→CUDA→NVENC, double-blit-avoided=%v)", n, fd, alreadyBlitted)
		}
	}
	return fd, err
}

// Destroy cleans up the provider and its frame capture resources.
func (p *Provider) Destroy() {
	unregisterProvider(p)
	if p.fc != nil {
		p.fc.Destroy()
		p.fc = nil
	}
	if p.zeroCopy != nil {
		p.zeroCopy.Destroy()
		p.zeroCopy = nil
	}
	if p.iface != nil {
		C.free(unsafe.Pointer(p.iface))
		p.iface = nil
	}
}

// ── C export callbacks ─────────────────────────────────────────────────────
// These are called from C shims registered in the retro_hw_render_interface_vulkan.

//export go_set_image
func go_set_image(handle unsafe.Pointer, image *C.struct_retro_vulkan_image,
	numSems C.uint32_t, sems *C.VkSemaphore, srcQF C.uint32_t) {
	p := lookupProvider(handle)
	if p == nil || image == nil {
		return
	}
	p.mu.Lock()
	p.currentImageView = image.image_view
	p.currentLayout = image.image_layout
	// Phase 3 fix: extract VkImage from VkImageViewCreateInfo.image.
	// The standard libretro Vulkan spec requires the core to populate
	// create_info.image with the underlying VkImage that was created by the
	// core using the VkDevice we provided in the render interface.
	// (The old CloudPlay-extension `image` field has been removed to match
	// the real spec exactly and avoid struct-layout mismatches.)
	p.currentImage = image.create_info.image
	p.syncIndex = (p.syncIndex + 1) & 1
	// Reset per-frame zero-copy blit flag so ReadFrameZeroCopy will blit
	// exactly once for this new image.
	p.zeroCopyBlitDone = false
	p.mu.Unlock()
}

//export go_get_sync_index
func go_get_sync_index(handle unsafe.Pointer) C.uint32_t {
	p := lookupProvider(handle)
	if p == nil {
		return 0
	}
	p.mu.Lock()
	idx := p.syncIndex
	p.mu.Unlock()
	return C.uint32_t(idx)
}

//export go_get_sync_index_mask
func go_get_sync_index_mask(handle unsafe.Pointer) C.uint32_t {
	// Double-buffered: indices 0 and 1 are valid.
	return C.uint32_t(0x3)
}

//export go_set_command_buffers
func go_set_command_buffers(handle unsafe.Pointer, num C.uint32_t, cmds *C.VkCommandBuffer) {
	// Phase 3 TODO: In the final zero-copy path the core provides its render
	// command buffers here so the host can append blit-to-zerocopy commands
	// before submission.  For now we accept without injecting extra work;
	// the BlitFrom call in ReadFrameZeroCopy uses a separate one-shot command
	// buffer submitted after the core has finished rendering.
	_ = num
	_ = cmds
}

//export go_wait_sync_index
func go_wait_sync_index(handle unsafe.Pointer) {
	p := lookupProvider(handle)
	if p == nil {
		return
	}
	// Wait only on the queue the core rendered into — not all device queues.
	// vkDeviceWaitIdle would stall compute/transfer queues too, causing the
	// per-frame stutter observed in Phase 3 staging.
	C.vkQueueWaitIdle(p.ctx.Queue)
}

//export go_lock_queue
func go_lock_queue(handle unsafe.Pointer) {
	p := lookupProvider(handle)
	if p == nil {
		return
	}
	p.queueMu.Lock()
}

//export go_unlock_queue
func go_unlock_queue(handle unsafe.Pointer) {
	p := lookupProvider(handle)
	if p == nil {
		return
	}
	p.queueMu.Unlock()
}

//export go_set_signal_semaphore
func go_set_signal_semaphore(handle unsafe.Pointer, sem C.VkSemaphore) {
	// Phase 2: not wired to an encoder sync path yet.
	_ = sem
}

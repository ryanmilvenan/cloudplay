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

// ── Bootstrap render interface for create_device phase ────────────────────
// During Dolphin's CreateDevice callback, VulkanContext::Create() creates a
// swapchain whose wrapper calls vulkan->get_sync_index_mask(vulkan->handle).
// At that point our real Provider doesn't exist yet. This bootstrap interface
// provides the minimal callbacks so the swapchain can be created without
// crashing on a NULL vulkan pointer.

static struct retro_hw_render_interface_vulkan g_bootstrap_iface;

static struct retro_hw_render_interface_vulkan* get_bootstrap_iface_ptr(void) {
    return &g_bootstrap_iface;
}

static void bootstrap_set_image(void *handle, const struct retro_vulkan_image *image,
    uint32_t num_semaphores, const VkSemaphore *semaphores, uint32_t src_queue_family) {}
static uint32_t bootstrap_get_sync_index(void *handle) { return 0; }
static uint32_t bootstrap_get_sync_index_mask(void *handle) { return 0x3; } // double buffer
static void bootstrap_set_command_buffers(void *handle, uint32_t num, const VkCommandBuffer *cmd) {}
static void bootstrap_wait_sync_index(void *handle) {}
static void bootstrap_lock_queue(void *handle) {}
static void bootstrap_unlock_queue(void *handle) {}
static void bootstrap_set_signal_semaphore(void *handle, VkSemaphore sem) {}

// Forward declaration — defined later in the headless surface stubs section.
static VKAPI_ATTR PFN_vkVoidFunction VKAPI_CALL cloudplay_GetInstanceProcAddr(
    VkInstance instance, const char *pName);

// init_bootstrap_interface sets up a minimal render interface with stub
// callbacks.  The core can use it during device creation.  Call this BEFORE
// call_create_device so the core's swapchain wrapper has a valid vulkan pointer.
static void init_bootstrap_interface(
    VkInstance instance, VkPhysicalDevice gpu, VkDevice device,
    VkQueue queue, unsigned queue_index)
{
    memset(&g_bootstrap_iface, 0, sizeof(g_bootstrap_iface));
    g_bootstrap_iface.interface_type    = RETRO_HW_RENDER_INTERFACE_VULKAN;
    g_bootstrap_iface.interface_version = 5;
    g_bootstrap_iface.handle            = (void*)0xDEAD; // non-NULL sentinel
    g_bootstrap_iface.instance          = instance;
    g_bootstrap_iface.gpu               = gpu;
    g_bootstrap_iface.device            = device ? device : VK_NULL_HANDLE;
    g_bootstrap_iface.get_device_proc_addr   = vkGetDeviceProcAddr;
    g_bootstrap_iface.get_instance_proc_addr = cloudplay_GetInstanceProcAddr;
    g_bootstrap_iface.queue             = queue ? queue : VK_NULL_HANDLE;
    g_bootstrap_iface.queue_index       = queue_index;
    g_bootstrap_iface.set_image            = bootstrap_set_image;
    g_bootstrap_iface.get_sync_index       = bootstrap_get_sync_index;
    g_bootstrap_iface.get_sync_index_mask  = bootstrap_get_sync_index_mask;
    g_bootstrap_iface.set_command_buffers  = bootstrap_set_command_buffers;
    g_bootstrap_iface.wait_sync_index      = bootstrap_wait_sync_index;
    g_bootstrap_iface.lock_queue           = bootstrap_lock_queue;
    g_bootstrap_iface.unlock_queue         = bootstrap_unlock_queue;
    g_bootstrap_iface.set_signal_semaphore = bootstrap_set_signal_semaphore;
    g_bootstrap_iface.negotiation_interface = &g_dummy_neg;
}

// ── Headless surface format stubs ──────────────────────────────────────────
// When using VK_EXT_headless_surface or an Xvfb-backed VK_KHR_xlib_surface,
// the NVIDIA driver may fail vkGetPhysicalDeviceSurfaceFormatsKHR with
// VK_ERROR_UNKNOWN (-13).  Dolphin's libretro Vulkan code resolves this
// function via get_instance_proc_addr and calls it to configure the swapchain.
//
// We intercept get_instance_proc_addr to return stubs for the two problematic
// surface query functions, providing sane defaults that let the core create
// its virtual swapchain without ever hitting the real driver surface queries.

static VkPhysicalDevice g_cloudplay_gpu = VK_NULL_HANDLE;
static VkSurfaceKHR     g_cloudplay_surface = VK_NULL_HANDLE;

static VKAPI_ATTR VkResult VKAPI_CALL cloudplay_GetPhysicalDeviceSurfaceFormatsKHR(
    VkPhysicalDevice physicalDevice, VkSurfaceKHR surface,
    uint32_t *pSurfaceFormatCount, VkSurfaceFormatKHR *pSurfaceFormats)
{
    // Try the real driver first — if it succeeds, use its answer.
    VkResult real = vkGetPhysicalDeviceSurfaceFormatsKHR(physicalDevice, surface,
                                                         pSurfaceFormatCount, pSurfaceFormats);
    if (real == VK_SUCCESS) return real;

    // Driver failed (headless/Xvfb) — return a sane default.
    if (!pSurfaceFormats) {
        *pSurfaceFormatCount = 1;
        return VK_SUCCESS;
    }
    if (*pSurfaceFormatCount >= 1) {
        pSurfaceFormats[0].format     = VK_FORMAT_B8G8R8A8_UNORM;
        pSurfaceFormats[0].colorSpace = VK_COLOR_SPACE_SRGB_NONLINEAR_KHR;
        *pSurfaceFormatCount = 1;
    }
    return VK_SUCCESS;
}

static VKAPI_ATTR VkResult VKAPI_CALL cloudplay_GetPhysicalDeviceSurfacePresentModesKHR(
    VkPhysicalDevice physicalDevice, VkSurfaceKHR surface,
    uint32_t *pPresentModeCount, VkPresentModeKHR *pPresentModes)
{
    VkResult real = vkGetPhysicalDeviceSurfacePresentModesKHR(physicalDevice, surface,
                                                              pPresentModeCount, pPresentModes);
    if (real == VK_SUCCESS) return real;

    if (!pPresentModes) {
        *pPresentModeCount = 1;
        return VK_SUCCESS;
    }
    if (*pPresentModeCount >= 1) {
        pPresentModes[0] = VK_PRESENT_MODE_FIFO_KHR;
        *pPresentModeCount = 1;
    }
    return VK_SUCCESS;
}

static VKAPI_ATTR VkResult VKAPI_CALL cloudplay_GetPhysicalDeviceSurfaceCapabilitiesKHR(
    VkPhysicalDevice physicalDevice, VkSurfaceKHR surface,
    VkSurfaceCapabilitiesKHR *pSurfaceCapabilities)
{
    // ALWAYS override capabilities — the real driver may return {1,1} extent
    // for headless/Xvfb surfaces, causing Dolphin to create 1×1 swapchain images.
    VkResult real = vkGetPhysicalDeviceSurfaceCapabilitiesKHR(physicalDevice, surface,
                                                              pSurfaceCapabilities);
    if (real != VK_SUCCESS) {
        memset(pSurfaceCapabilities, 0, sizeof(*pSurfaceCapabilities));
        pSurfaceCapabilities->maxImageArrayLayers = 1;
        pSurfaceCapabilities->supportedTransforms = VK_SURFACE_TRANSFORM_IDENTITY_BIT_KHR;
        pSurfaceCapabilities->currentTransform    = VK_SURFACE_TRANSFORM_IDENTITY_BIT_KHR;
        pSurfaceCapabilities->supportedCompositeAlpha = VK_COMPOSITE_ALPHA_OPAQUE_BIT_KHR;
    }
    pSurfaceCapabilities->minImageCount = 2;
    pSurfaceCapabilities->maxImageCount = 8;
    pSurfaceCapabilities->currentExtent.width  = 640;
    pSurfaceCapabilities->currentExtent.height = 528;
    pSurfaceCapabilities->minImageExtent.width  = 1;
    pSurfaceCapabilities->minImageExtent.height = 1;
    pSurfaceCapabilities->maxImageExtent.width  = 4096;
    pSurfaceCapabilities->maxImageExtent.height = 4096;
    pSurfaceCapabilities->supportedUsageFlags =
        VK_IMAGE_USAGE_COLOR_ATTACHMENT_BIT | VK_IMAGE_USAGE_TRANSFER_SRC_BIT |
        VK_IMAGE_USAGE_TRANSFER_DST_BIT | VK_IMAGE_USAGE_SAMPLED_BIT;
    return VK_SUCCESS;
}

// Intercepting get_instance_proc_addr: if the core asks for surface query
// functions, return our stubs.  Everything else goes to the real loader.
static VKAPI_ATTR PFN_vkVoidFunction VKAPI_CALL cloudplay_GetInstanceProcAddr(
    VkInstance instance, const char *pName)
{
    if (strcmp(pName, "vkGetPhysicalDeviceSurfaceFormatsKHR") == 0)
        return (PFN_vkVoidFunction)cloudplay_GetPhysicalDeviceSurfaceFormatsKHR;
    if (strcmp(pName, "vkGetPhysicalDeviceSurfacePresentModesKHR") == 0)
        return (PFN_vkVoidFunction)cloudplay_GetPhysicalDeviceSurfacePresentModesKHR;
    if (strcmp(pName, "vkGetPhysicalDeviceSurfaceCapabilitiesKHR") == 0)
        return (PFN_vkVoidFunction)cloudplay_GetPhysicalDeviceSurfaceCapabilitiesKHR;
    return vkGetInstanceProcAddr(instance, pName);
}

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
    // Use our intercepting get_instance_proc_addr so headless surface queries
    // return sane defaults even when the real driver fails.
    iface->get_device_proc_addr   = vkGetDeviceProcAddr;
    iface->get_instance_proc_addr = cloudplay_GetInstanceProcAddr;

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

    // Runtime contract: when the frontend does not implement a negotiation
    // interface for the core to call later, this field should be NULL.
    // Keep the bootstrap interface using g_dummy_neg during device-creation,
    // but expose the real runtime interface honestly here.
    iface->negotiation_interface = NULL;
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
	waitSkipCount      int64 // counter for go_wait_sync_index skip diagnostics
	diagSetImageCount  int64 // counter for go_set_image diagnostics
	diagReadFrameCount int64 // counter for ReadFrame diagnostics
	diagSignalSemCount int64 // counter for go_set_signal_semaphore diagnostics
	diagCmdBufCount    int64 // counter for go_set_command_buffers diagnostics
	diagGetSyncCount   int64 // counter for go_get_sync_index diagnostics
	diagGetMaskCount   int64 // counter for go_get_sync_index_mask diagnostics
	diagWaitSyncCount  int64 // counter for go_wait_sync_index diagnostics
	diagLockQueueCount int64 // counter for go_lock_queue diagnostics
	diagUnlockQueueCount int64 // counter for go_unlock_queue diagnostics
	diagExportProbeCnt int64 // one-shot export-buffer probe after sync blit
	diagRepeatedSetProbe int64 // one-shot repeated-image probe in go_set_image
	diagQueueProbeCount int64 // one-shot repeated-image CPU probe in unlock_queue
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

	// framesSeen counts how many set_image calls have been received.
	framesSeen uint32

	// imageSetSeq increments for every go_set_image call and lets us correlate
	// a specific exported black frame with the exact VkImage handle the core
	// most recently handed us.
	imageSetSeq uint64

	// seenImages tracks a small rolling set of VkImage handles so we can detect
	// when LRPS2 starts reusing images from its pool. We want to probe the first
	// repeated image rather than the very first freshly-created handle.
	seenImages           [16]C.VkImage
	seenImageCount       int
	currentImageRepeated bool
	pendingQueueProbe    bool

	// cachedPixels holds the most recent readback result, populated during
	// go_wait_sync_index when the queue is guaranteed idle.  On the
	// negotiated-device path this is the only safe time to submit readback
	// commands (the core owns the queue and wraps vkQueueSubmit).
	cachedPixels []byte

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

	// waitSems are the semaphores the core asked the frontend to wait on
	// before consuming the current frame image. We currently wire these into
	// the synchronous zero-copy blit path, which is the active live path for
	// LRPS2 black-frame repros.
	waitSems     [8]C.VkSemaphore
	waitSemCount int
	waitSrcQF    C.uint32_t

	// Interface struct we hand to the core via GET_HW_RENDER_INTERFACE.
	// Stored in C-owned memory because the libretro core retains this pointer.
	iface  *C.struct_retro_hw_render_interface_vulkan
	handle cgo.Handle

	// queueMu guards lock/unlock_queue calls.
	queueMu sync.Mutex

	// readbackFailed is set permanently after the first DEVICE_LOST from
	// readback, preventing further attempts that would cascade errors.
	readbackFailed bool
}

// InitBootstrapInterface creates a minimal render interface with stub callbacks.
// Call this BEFORE the core's create_device so that Dolphin's internal swapchain
// wrapper has a valid vulkan pointer during device creation.
func InitBootstrapInterface(instance, gpu unsafe.Pointer) {
	C.init_bootstrap_interface(
		(C.VkInstance)(instance),
		(C.VkPhysicalDevice)(gpu),
		nil, // device not yet created
		nil, // queue not yet created
		0,   // queue_index
	)
}

// BootstrapInterfacePtr returns the address of the bootstrap render interface.
// Use this as the response to GET_HW_RENDER_INTERFACE when the real Provider
// hasn't been created yet.
func BootstrapInterfacePtr() unsafe.Pointer {
	return unsafe.Pointer(C.get_bootstrap_iface_ptr())
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
		log.Printf("[cloudplay diag] ensureZeroCopy: realloc old=%dx%d new=%dx%d", p.zeroCopy.width, p.zeroCopy.height, w, h)
		p.zeroCopy.Destroy()
		p.zeroCopy = nil
	} else {
		log.Printf("[cloudplay diag] ensureZeroCopy: alloc new=%dx%d", w, h)
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
	frames := p.framesSeen
	p.mu.Unlock()

	// On the negotiated-device path (Dolphin, Flycast, etc.), we CANNOT do
	// our own queue submissions — the core owns the queue and wraps
	// vkQueueSubmit.  Any readback we attempt via FrameCapture causes
	// VK_ERROR_DEVICE_LOST which permanently kills the VkDevice.
	//
	// Instead, use the cached readback from go_wait_sync_index (which runs
	// when the queue is guaranteed idle between frames).
	if p.ctx.externalHandles {
		p.mu.Lock()
		cached := p.cachedPixels
		p.mu.Unlock()
		if cached != nil {
			// DIAG: log what cachedPixels contains when consumed
			drn := atomic.AddInt64(&diagReadFrameCount, 1)
			if drn%60 == 1 && len(cached) >= 16 {
				nonZero := 0
				for i := 0; i < len(cached) && i < 4000; i += 4 {
					if cached[i] != 0 || cached[i+1] != 0 || cached[i+2] != 0 { nonZero++ }
				}
				log.Printf("[cloudplay diag] ReadFrame(extHandles) frame=%d cached_len=%d first16=%v nonZeroPixels_in_4k=%d w=%d h=%d",
					drn, len(cached), cached[:16], nonZero, w, h)
			}
			return cached, nil
		}
		// No cached readback yet (first few frames) — return blank.
		log.Printf("[cloudplay diag] ReadFrame(extHandles) cachedPixels=nil, returning blank w=%d h=%d", w, h)
		return make([]byte, int(w*h*4)), nil
	}

	// Non-negotiated path: skip first few frames during init.
	if frames < 4 {
		return make([]byte, int(w*h*4)), nil
	}

	if ft != nil {
		ft.markWaitDone()
	}

	if img == nil {
		// DIAG: log nil image — this is the key breakage point
		rn := atomic.AddInt64(&diagReadFrameCount, 1)
		if rn%60 == 1 {
			log.Printf("[cloudplay diag] ReadFrame frame=%d img=nil returning blank frame w=%d h=%d", rn, w, h)
		}
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
				p.queueMu.Lock()
				blitErr := p.zeroCopy.BlitFrom(img, layout)
				p.queueMu.Unlock()
				if blitErr == nil {
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

	// Hold the queue lock during readback — Dolphin's vkQueueSubmit wrapper
	// expects all queue submissions to go through lock_queue/unlock_queue.
	// Without this, concurrent submissions cause VK_ERROR_DEVICE_LOST.
	p.queueMu.Lock()
	pixels, err := p.fc.Readback(img, layout)
	p.queueMu.Unlock()
	if ft != nil {
		ft.markReadDone()
		ft.emit(zeroCopyActive)
	}

	// DIAG: log pixel data quality every 60th call
	drn := atomic.AddInt64(&diagReadFrameCount, 1)
	if drn%60 == 1 {
		if err != nil {
			log.Printf("[cloudplay diag] ReadFrame frame=%d readback_err=%v", drn, err)
		} else if len(pixels) >= 16 {
			log.Printf("[cloudplay diag] ReadFrame frame=%d pixels_len=%d first16=%v", drn, len(pixels), pixels[:16])
		} else {
			log.Printf("[cloudplay diag] ReadFrame frame=%d pixels_len=%d (short)", drn, len(pixels))
		}
	}

	return pixels, err
}

// ReadFrameZeroCopy performs the Phase 3 GPU-to-GPU blit into the exportable
// buffer and returns the buffer's fd plus exported allocation size for CUDA
// import. It does NOT copy pixels to the CPU.
//
// Returns (-1, err) if Phase 3 is unavailable or the blit fails.
//
// Typical call flow (nanoarch_vulkan.go, Phase 3 fully wired):
//
//	fd, size, err := provider.ReadFrameZeroCopy(w, h)
//	// → CUDA: cuMemImportFromShareableHandle(fd, size)
//	// → NVENC: nvenc.EncodeFromDevPtr(cudaPtr, size)
// WaitZeroCopyBlit waits for the most recent async BlitFrom to complete.
// No-op if zero-copy is not active or no blit is pending.
func (p *Provider) WaitZeroCopyBlit() error {
	if p.zeroCopy == nil {
		return nil
	}
	return p.zeroCopy.WaitBlit()
}

func (p *Provider) ReadFrameZeroCopy(w, h uint32) (fd int, size uint64, err error) {
	if !p.zeroCopyAvailable() {
		return -1, 0, fmt.Errorf("zerocopy: not available on this device")
	}

	p.mu.Lock()
	img := p.currentImage
	layout := p.currentLayout
	imgSeq := p.imageSetSeq
	imgRepeated := p.currentImageRepeated
	waitCount := p.waitSemCount
	waitSrcQF := p.waitSrcQF
	waitSems := make([]C.VkSemaphore, waitCount)
	copy(waitSems, p.waitSems[:waitCount])
	p.mu.Unlock()

	if img == nil {
		return -1, 0, fmt.Errorf("zerocopy: no current image")
	}

	if err = p.ensureZeroCopy(w, h); err != nil {
		return -1, 0, err
	}

	// Avoid double-blit: ReadFrame may have already blitted this frame as a
	// side effect (when zero-copy is available).  Check zeroCopyBlitDone.
	p.mu.Lock()
	alreadyBlitted := p.zeroCopyBlitDone
	zcW, zcH := uint32(0), uint32(0)
	if p.zeroCopy != nil {
		zcW, zcH = p.zeroCopy.width, p.zeroCopy.height
	}
	p.mu.Unlock()

	if alreadyBlitted && (zcW != w || zcH != h) {
		log.Printf("[cloudplay diag] ReadFrameZeroCopy: alreadyBlitted with size mismatch requested=%dx%d zc=%dx%d", w, h, zcW, zcH)
	}

	if !alreadyBlitted {
		log.Printf("[cloudplay diag] ReadFrameZeroCopy: sync BlitFrom seq=%d image=%p requested=%dx%d layout=%d zc=%dx%d waitSems=%d srcQF=%d", imgSeq, img, w, h, int(layout), zcW, zcH, len(waitSems), uint32(waitSrcQF))
		if err = p.zeroCopy.BlitFromWait(img, layout, waitSems); err != nil {
			return -1, 0, err
		}
		if imgRepeated && atomic.CompareAndSwapInt64(&diagExportProbeCnt, 0, 1) {
			probe, probeErr := p.zeroCopy.ProbePrefix(4096)
			log.Printf("[cloudplay diag] ReadFrameZeroCopy: export probe tag seq=%d image=%p repeated=%v", imgSeq, img, imgRepeated)
			if probeErr != nil {
				log.Printf("[cloudplay diag] ReadFrameZeroCopy: export buffer probe failed: %v", probeErr)
			} else {
				nonZero := 0
				for i := 0; i+3 < len(probe); i += 4 {
					if probe[i] != 0 || probe[i+1] != 0 || probe[i+2] != 0 {
						nonZero++
					}
				}
				if len(probe) >= 16 {
					log.Printf("[cloudplay diag] ReadFrameZeroCopy: export buffer probe len=%d first16=%v nonZeroPixels_in_4k=%d", len(probe), probe[:16], nonZero)
				} else {
					log.Printf("[cloudplay diag] ReadFrameZeroCopy: export buffer probe len=%d nonZeroPixels_in_4k=%d", len(probe), nonZero)
				}
			}
		}
		p.mu.Lock()
		p.zeroCopyBlitDone = true
		p.mu.Unlock()
	} else {
		log.Printf("[cloudplay diag] ReadFrameZeroCopy: reusing async blit requested=%dx%d zc=%dx%d", w, h, zcW, zcH)
	}

	fd, err = p.zeroCopy.ExportMemoryFd()
	size = p.zeroCopy.Size()
	if err == nil {
		// Confirm zero-copy hot-path is active every 300 frames.
		n := atomic.AddInt64(&frameCount, 1)
		if n%300 == 0 {
			log.Printf("[cloudplay/zero-copy] frame=%d fd=%d active=true (GPU→CUDA→NVENC, double-blit-avoided=%v)", n, fd, alreadyBlitted)
		}
	}
	return fd, size, err
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
	prevImage := p.currentImage
	prevImageView := p.currentImageView
	repeatedImage := false
	for i := 0; i < p.seenImageCount; i++ {
		if p.seenImages[i] == image.create_info.image {
			repeatedImage = true
			break
		}
	}
	if !repeatedImage {
		if p.seenImageCount < len(p.seenImages) {
			p.seenImages[p.seenImageCount] = image.create_info.image
			p.seenImageCount++
		} else {
			copy(p.seenImages[0:], p.seenImages[1:])
			p.seenImages[len(p.seenImages)-1] = image.create_info.image
		}
	}
	p.currentImageRepeated = repeatedImage
	p.pendingQueueProbe = repeatedImage
	p.currentImage = image.create_info.image
	p.syncIndex = (p.syncIndex + 1) & 1
	p.framesSeen++
	p.imageSetSeq++
	curSeq := p.imageSetSeq
	// Reset per-frame zero-copy blit flag so ReadFrameZeroCopy will blit
	// exactly once for this new image.
	p.zeroCopyBlitDone = false
	p.waitSemCount = 0
	p.waitSrcQF = srcQF
	if sems != nil && numSems > 0 {
		count := int(numSems)
		if count > len(p.waitSems) {
			count = len(p.waitSems)
		}
		for i, sem := range unsafe.Slice(sems, count) {
			p.waitSems[i] = sem
		}
		p.waitSemCount = count
	}
	zcW, zcH := uint32(0), uint32(0)
	if p.zeroCopy != nil {
		zcW, zcH = p.zeroCopy.width, p.zeroCopy.height
	}
	waitSemsCopy := make([]C.VkSemaphore, p.waitSemCount)
	copy(waitSemsCopy, p.waitSems[:p.waitSemCount])

	// DIAG: sparse log to verify image is being set correctly
	_ = p.currentImage
	_ = p.currentLayout
	_ = p.currentImageView
	p.mu.Unlock()

	n := atomic.AddInt64(&diagSetImageCount, 1)
	if n <= 12 {
		// Dump raw bytes of the retro_vulkan_image struct to diagnose alignment
		structSize := unsafe.Sizeof(*image)
		rawBytes := unsafe.Slice((*byte)(unsafe.Pointer(image)), structSize)
		// Log first 96 bytes (full struct) as hex
		hexStr := fmt.Sprintf("%x", rawBytes[:min(96, len(rawBytes))])
		ci := image.create_info
		log.Printf("[cloudplay diag] go_set_image ts=%d frame=%d seq=%d imageChanged=%v imageRepeated=%v prevImage=%p image_view=%p prevView=%p layout=%d ci.sType=%d ci.image=%p ci.format=%d ci.viewType=%d baseMip=%d levelCount=%d baseLayer=%d layerCount=%d waitSems=%d srcQF=%d hex=%s",
			time.Now().UnixNano(), n, curSeq, prevImage != ci.image || prevImageView != image.image_view, repeatedImage, prevImage, image.image_view, prevImageView, int(image.image_layout), int(ci.sType), ci.image, int(ci.format), int(ci.viewType),
			int(ci.subresourceRange.baseMipLevel), int(ci.subresourceRange.levelCount), int(ci.subresourceRange.baseArrayLayer), int(ci.subresourceRange.layerCount),
			int(numSems), uint32(srcQF), hexStr)
	}

	if repeatedImage && zcW > 0 && zcH > 0 {
		if atomic.CompareAndSwapInt64(&diagRepeatedSetProbe, 0, 1) {
			tempZC, tempErr := NewZeroCopyBuffer(p.ctx, zcW, zcH)
			if tempErr != nil {
				log.Printf("[cloudplay diag] go_set_image repeated probe create failed: %v", tempErr)
			} else {
				defer tempZC.Destroy()
				if tempErr = tempZC.BlitFromWait(image.create_info.image, image.image_layout, waitSemsCopy); tempErr != nil {
					log.Printf("[cloudplay diag] go_set_image repeated probe blit failed: %v", tempErr)
				} else {
					probe, probeErr := tempZC.ProbePrefix(4096)
					if probeErr != nil {
						log.Printf("[cloudplay diag] go_set_image repeated probe read failed: %v", probeErr)
					} else {
						nonZero := 0
						for i := 0; i+3 < len(probe); i += 4 {
							if probe[i] != 0 || probe[i+1] != 0 || probe[i+2] != 0 {
								nonZero++
							}
						}
						if len(probe) >= 16 {
							log.Printf("[cloudplay diag] go_set_image repeated probe seq=%d image=%p dims=%dx%d first16=%v nonZeroPixels_in_4k=%d", curSeq, image.create_info.image, zcW, zcH, probe[:16], nonZero)
						} else {
							log.Printf("[cloudplay diag] go_set_image repeated probe seq=%d image=%p dims=%dx%d nonZeroPixels_in_4k=%d", curSeq, image.create_info.image, zcW, zcH, nonZero)
						}
					}
				}
			}
		}
	}
}

//export go_get_sync_index
func go_get_sync_index(handle unsafe.Pointer) C.uint32_t {
	p := lookupProvider(handle)
	if p == nil {
		return 0
	}
	p.mu.Lock()
	idx := p.syncIndex
	frames := p.framesSeen
	img := p.currentImage
	seq := p.imageSetSeq
	p.mu.Unlock()
	n := atomic.AddInt64(&diagGetSyncCount, 1)
	if n <= 20 {
		log.Printf("[cloudplay diag] go_get_sync_index ts=%d call=%d idx=%d frames=%d seq=%d image=%p", time.Now().UnixNano(), n, idx, frames, seq, img)
	}
	return C.uint32_t(idx)
}

//export go_get_sync_index_mask
func go_get_sync_index_mask(handle unsafe.Pointer) C.uint32_t {
	n := atomic.AddInt64(&diagGetMaskCount, 1)
	if n <= 20 {
		log.Printf("[cloudplay diag] go_get_sync_index_mask ts=%d call=%d mask=0x3", time.Now().UnixNano(), n)
	}
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
	n := atomic.AddInt64(&diagCmdBufCount, 1)
	if n <= 12 {
		var first, second C.VkCommandBuffer
		if cmds != nil && num > 0 {
			s := unsafe.Slice(cmds, int(num))
			first = s[0]
			if len(s) > 1 {
				second = s[1]
			}
		}
		log.Printf("[cloudplay diag] go_set_command_buffers call=%d num=%d first=%p second=%p", n, uint32(num), first, second)
	}
}

//export go_wait_sync_index
func go_wait_sync_index(handle unsafe.Pointer) {
	p := lookupProvider(handle)
	if p == nil {
		return
	}
	p.mu.Lock()
	preIdx := p.syncIndex
	preFrames := p.framesSeen
	preImg := p.currentImage
	preSeq := p.imageSetSeq
	p.mu.Unlock()
	nWait := atomic.AddInt64(&diagWaitSyncCount, 1)
	if nWait <= 20 {
		log.Printf("[cloudplay diag] go_wait_sync_index enter ts=%d call=%d idx=%d frames=%d seq=%d image=%p", time.Now().UnixNano(), nWait, preIdx, preFrames, preSeq, preImg)
	}

	// On the negotiated-device path (Dolphin, Flycast, etc.), this callback
	// is invoked by Dolphin's vkAcquireNextImageKHR wrapper between frames.
	// The queue should be idle at this point.
	//
	// We try a minimal readback: just submit our command buffer with the blit.
	// If it causes DEVICE_LOST, we disable readback permanently for this
	// session rather than retrying every frame.
	if p.ctx.externalHandles {
		if p.readbackFailed {
			return // Permanently disabled after first DEVICE_LOST
		}

		p.mu.Lock()
		img := p.currentImage
		layout := p.currentLayout
		frames := p.framesSeen
		p.mu.Unlock()

		if img == nil || frames < 10 {
			n := atomic.AddInt64(&waitSkipCount, 1)
			if n <= 10 {
				log.Printf("[cloudplay diag] go_wait_sync_index: skip imgNil=%v frames=%d zeroCopyAvailable=%v", img == nil, frames, p.zeroCopyAvailable())
			}
			return
		}

		w := uint32(640)
		h := uint32(528)

		p.queueMu.Lock()
		defer p.queueMu.Unlock()

		// Phase 3 fast path: when zero-copy is active, ONLY do the blit —
		// skip vkDeviceWaitIdle and the CPU readback entirely.
		// BlitFrom→submitOneShotFenced already handles its own fence sync.
		// This reduces per-frame GPU sync from 3 stalls to 1, fixing the
		// burst/stutter pattern caused by accumulated stall latency.
		if p.zeroCopyAvailable() {
			zcErr := p.ensureZeroCopy(w, h)
			if zcErr == nil {
				// Async blit: submit and return immediately.
				// The fence will be waited on by the encode path (ticker goroutine).
				blitErr := p.zeroCopy.BlitFromAsync(img, layout)
				if blitErr == nil {
					p.mu.Lock()
					p.zeroCopyBlitDone = true
					p.mu.Unlock()
					n := atomic.AddInt64(&frameCount, 1)
					if n <= 5 || n%300 == 0 {
						log.Printf("[cloudplay] go_wait_sync_index: zero-copy BlitFromAsync submitted frame=%d", n)
					}
					return // fast path: no CPU readback needed
				}
				log.Printf("[cloudplay] go_wait_sync_index: zero-copy BlitFromAsync err=%v, falling back to CPU readback", blitErr)
			}
			// BlitFromAsync failed — fall through to CPU readback path below.
		}

		// CPU readback path (fallback when zero-copy is not available or blit failed).
		C.vkDeviceWaitIdle(p.ctx.Device)

		if p.fc == nil || p.fc.width != w || p.fc.height != h {
			if p.fc != nil { p.fc.Destroy() }
			fc, err := NewFrameCapture(p.ctx, w, h)
			if err != nil {
				log.Printf("[cloudplay] go_wait_sync_index: NewFrameCapture failed: %v", err)
				p.readbackFailed = true
				return
			}
			p.fc = fc
		}

		pixels, err := p.fc.Readback(img, layout)
		if err != nil {
			log.Printf("[cloudplay] go_wait_sync_index: Readback failed: %v", err)
			p.readbackFailed = true
			return
		}

		if len(pixels) > 0 {
			cp := make([]byte, len(pixels))
			copy(cp, pixels)
			p.mu.Lock()
			p.cachedPixels = cp
			p.mu.Unlock()
			n := atomic.AddInt64(&frameCount, 1)
			if n <= 5 || n%300 == 0 {
				log.Printf("[cloudplay] go_wait_sync_index: CPU Readback %d bytes, first16=%v", len(cp), cp[:min(16, len(cp))])
			}
		}
		return
	}

	// Non-negotiated path: safe to wait on our own queue.
	C.vkQueueWaitIdle(p.ctx.Queue)
}

//export go_lock_queue
func go_lock_queue(handle unsafe.Pointer) {
	p := lookupProvider(handle)
	if p == nil {
		return
	}
	n := atomic.AddInt64(&diagLockQueueCount, 1)
	if n <= 20 {
		log.Printf("[cloudplay diag] go_lock_queue ts=%d call=%d", time.Now().UnixNano(), n)
	}
	p.queueMu.Lock()
}

//export go_unlock_queue
func go_unlock_queue(handle unsafe.Pointer) {
	p := lookupProvider(handle)
	if p == nil {
		return
	}
	p.mu.Lock()
	img := p.currentImage
	layout := p.currentLayout
	seq := p.imageSetSeq
	repeated := p.currentImageRepeated
	pendingProbe := p.pendingQueueProbe
	zcW, zcH := uint32(0), uint32(0)
	if p.zeroCopy != nil {
		zcW, zcH = p.zeroCopy.width, p.zeroCopy.height
	}
	if pendingProbe && repeated && img != nil && zcW > 0 && zcH > 0 {
		p.pendingQueueProbe = false
	} else {
		pendingProbe = false
	}
	p.mu.Unlock()

	if pendingProbe && atomic.CompareAndSwapInt64(&diagQueueProbeCount, 0, 1) {
		tempFC, fcErr := NewFrameCapture(p.ctx, zcW, zcH)
		if fcErr != nil {
			log.Printf("[cloudplay diag] go_unlock_queue repeated CPU probe create failed: %v", fcErr)
		} else {
			defer tempFC.Destroy()
			pixels, readErr := tempFC.Readback(img, layout)
			if readErr != nil {
				log.Printf("[cloudplay diag] go_unlock_queue repeated CPU probe read failed: %v", readErr)
			} else {
				nonZero := 0
				limit := len(pixels)
				if limit > 4096 {
					limit = 4096
				}
				for i := 0; i+3 < limit; i += 4 {
					if pixels[i] != 0 || pixels[i+1] != 0 || pixels[i+2] != 0 {
						nonZero++
					}
				}
				if len(pixels) >= 16 {
					log.Printf("[cloudplay diag] go_unlock_queue repeated CPU probe seq=%d image=%p dims=%dx%d first16=%v nonZeroPixels_in_4k=%d", seq, img, zcW, zcH, pixels[:16], nonZero)
				} else {
					log.Printf("[cloudplay diag] go_unlock_queue repeated CPU probe seq=%d image=%p dims=%dx%d nonZeroPixels_in_4k=%d", seq, img, zcW, zcH, nonZero)
				}
			}
		}
	}

	n := atomic.AddInt64(&diagUnlockQueueCount, 1)
	if n <= 20 {
		log.Printf("[cloudplay diag] go_unlock_queue ts=%d call=%d", time.Now().UnixNano(), n)
	}
	p.queueMu.Unlock()
}

//export go_set_signal_semaphore
func go_set_signal_semaphore(handle unsafe.Pointer, sem C.VkSemaphore) {
	// Phase 2: not wired to an encoder sync path yet.
	n := atomic.AddInt64(&diagSignalSemCount, 1)
	if n <= 10 {
		log.Printf("[cloudplay diag] go_set_signal_semaphore call=%d sem=%p", n, sem)
	}
}

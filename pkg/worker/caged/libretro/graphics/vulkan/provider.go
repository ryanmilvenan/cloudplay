//go:build vulkan

package vulkan

/*
#cgo LDFLAGS: -lvulkan
#include "libretro_vulkan.h"
#include <stdlib.h>
#include <string.h>

// ── C-callable shim functions that proxy back into Go ──────────────────────
// These have the exact signatures required by the libretro Vulkan interface
// and are registered as function pointers in the render interface struct.

extern void   go_set_image(void *handle, const struct retro_vulkan_image *image,
                           uint32_t num_sems, const VkSemaphore *sems, uint32_t src_qf);
extern uint32_t go_get_sync_index(void *handle);
extern uint32_t go_get_sync_index_mask(void *handle);
extern void   go_set_command_buffers(void *handle, uint32_t num, const VkCommandBuffer *cmds);
extern void   go_wait_sync_index(void *handle, unsigned index);
extern void   go_lock_queue(void *handle);
extern void   go_unlock_queue(void *handle);
extern void   go_set_signal_semaphore(void *handle, VkSemaphore sem);

// Build the runtime interface struct, wiring in the shim callbacks.
static struct retro_hw_render_interface_vulkan make_vulkan_interface(
    void          *handle,
    VkInstance     instance,
    VkPhysicalDevice gpu,
    VkDevice       device,
    VkQueue        queue,
    unsigned       queue_index)
{
    struct retro_hw_render_interface_vulkan iface = {0};
    iface.interface_type    = RETRO_HW_RENDER_INTERFACE_VULKAN;
    iface.interface_version = 1;
    iface.handle            = handle;
    iface.instance          = instance;
    iface.gpu               = gpu;
    iface.device            = device;
    iface.queue             = queue;
    iface.queue_index       = queue_index;

    iface.set_image              = go_set_image;
    iface.get_sync_index         = go_get_sync_index;
    iface.get_sync_index_mask    = go_get_sync_index_mask;
    iface.set_command_buffers    = go_set_command_buffers;
    iface.wait_sync_index        = go_wait_sync_index;
    iface.lock_queue             = go_lock_queue;
    iface.unlock_queue           = go_unlock_queue;
    iface.set_signal_semaphore   = go_set_signal_semaphore;
    return iface;
}
*/
import "C"
import (
	"sync"
	"unsafe"
)

// ── Interface version constants (match libretro_vulkan.h) ─────────────────

const (
	RetroHWRenderInterfaceVulkan                    = 0
	RetroHWRenderContextNegotiationInterfaceVulkan  = 0
	NegotiationInterfaceVulkanVersion               = 1
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

	// frameCapture is created on-demand when the first image arrives.
	fc *FrameCapture

	// Interface struct we hand to the core via GET_HW_RENDER_INTERFACE.
	iface C.struct_retro_hw_render_interface_vulkan

	// queueMu guards lock/unlock_queue calls.
	queueMu sync.Mutex
}

// providerRegistry maps a *Provider (as unsafe.Pointer handle) to itself.
// The libretro C callbacks carry this opaque handle back to us.
var (
	providersMu sync.RWMutex
	providers   = map[unsafe.Pointer]*Provider{}
)

func registerProvider(p *Provider) unsafe.Pointer {
	h := unsafe.Pointer(p)
	providersMu.Lock()
	providers[h] = p
	providersMu.Unlock()
	return h
}

func lookupProvider(handle unsafe.Pointer) *Provider {
	providersMu.RLock()
	p := providers[handle]
	providersMu.RUnlock()
	return p
}

func unregisterProvider(p *Provider) {
	h := unsafe.Pointer(p)
	providersMu.Lock()
	delete(providers, h)
	providersMu.Unlock()
}

// NewProvider creates a Provider that wraps an existing Vulkan context.
func NewProvider(ctx *Context) (*Provider, error) {
	p := &Provider{
		ctx:           ctx,
		currentLayout: C.VK_IMAGE_LAYOUT_UNDEFINED,
	}

	handle := registerProvider(p)

	// Build the interface struct that the core will query via
	// RETRO_ENVIRONMENT_GET_HW_RENDER_INTERFACE.
	p.iface = C.make_vulkan_interface(
		handle,
		ctx.Instance,
		ctx.PhysDevice,
		ctx.Device,
		ctx.Queue,
		C.unsigned(ctx.QueueFamily),
	)

	return p, nil
}

// Interface returns a pointer to the retro_hw_render_interface_vulkan struct.
// Pass this pointer to the core in response to GET_HW_RENDER_INTERFACE.
func (p *Provider) Interface() *C.struct_retro_hw_render_interface_vulkan {
	return &p.iface
}

// CurrentImage returns the most recently set VkImage and its layout, if any.
func (p *Provider) CurrentImage() (img C.VkImage, layout C.VkImageLayout, view C.VkImageView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentImage, p.currentLayout, p.currentImageView
}

// ReadFrame copies the current rendered frame to CPU memory and returns RGBA bytes.
// w and h must match the actual rendered dimensions.
func (p *Provider) ReadFrame(w, h uint32) ([]byte, error) {
	p.mu.Lock()
	img := p.currentImage
	layout := p.currentLayout
	p.mu.Unlock()

	if img == nil {
		// No image yet — return a blank frame.
		size := int(w * h * 4)
		return make([]byte, size), nil
	}

	// Lazily create (or recreate on resize) the frame capture helper.
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

	return p.fc.Readback(img, layout)
}

// Destroy cleans up the provider and its frame capture resources.
func (p *Provider) Destroy() {
	unregisterProvider(p)
	if p.fc != nil {
		p.fc.Destroy()
		p.fc = nil
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
	// Use the explicit image field if the core provided it (CloudPlay extension).
	// Standard libretro cores will leave image.image as nil; ReadFrame handles
	// that by returning a blank frame until Phase 2b wires up the core image.
	p.currentImage = image.image
	// The render dimensions are supplied by the caller of ReadFrame (nanoarch)
	// via the w/h parameters from retro_system_av_info — we don't need to
	// parse them out of the image view create_info here.
	p.syncIndex = (p.syncIndex + 1) & 1
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
	// Phase 2: we don't schedule additional work into the core's command
	// stream yet — that's Phase 3 (zero-copy).  Accept but ignore.
}

//export go_wait_sync_index
func go_wait_sync_index(handle unsafe.Pointer, index C.unsigned) {
	p := lookupProvider(handle)
	if p == nil {
		return
	}
	// Wait for the device to be idle to guarantee the requested sync index
	// is no longer in flight.  This is conservative but correct for Phase 2.
	C.vkDeviceWaitIdle(p.ctx.Device)
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

//go:build vulkan

// Package vulkan provides a headless Vulkan rendering pipeline for libretro
// HW render cores.  It mirrors the public API of the GL pipeline so that
// nanoarch.go can switch between the two at build time via the `vulkan` tag.
package vulkan

/*
#cgo LDFLAGS: -lvulkan
#include "libretro_vulkan.h"
*/
import "C"
import (
	"fmt"
	"unsafe"
)

// Config holds the parameters needed to initialise the Vulkan context.
type Config struct {
	// Width and Height are the maximum framebuffer dimensions the core may
	// render at (from retro_get_system_av_info.max_width/max_height).
	Width  uint32
	Height uint32
}

// VulkanContext is the top-level object that owns the Vulkan context,
// the libretro provider, and the frame-capture staging buffer.
// It exposes the same operations that the GL path provides so that
// nanoarch.go can call them uniformly via the RenderPipeline interface.
type VulkanContext struct {
	cfg      Config
	ctx      *Context
	provider *Provider
	// surface is the headless VkSurfaceKHR created for the negotiation path.
	// Nil when using the non-negotiated device path (no surface needed there).
	surface *HeadlessSurface
}

// NewVulkanContext creates a headless Vulkan context and provider.
// Call Deinit() when done.
func NewVulkanContext(cfg Config) (*VulkanContext, error) {
	ctx, err := NewContext()
	if err != nil {
		return nil, fmt.Errorf("vulkan: context: %w", err)
	}

	prov, err := NewProvider(ctx)
	if err != nil {
		ctx.Destroy()
		return nil, fmt.Errorf("vulkan: provider: %w", err)
	}

	return &VulkanContext{
		cfg:      cfg,
		ctx:      ctx,
		provider: prov,
	}, nil
}

// NegotiationResult holds the Vulkan handles created by the core during
// RETRO_HW_RENDER_CONTEXT_NEGOTIATION.
type NegotiationResult struct {
	Instance    unsafe.Pointer // VkInstance
	PhysDevice  unsafe.Pointer // VkPhysicalDevice
	Device      unsafe.Pointer // VkDevice
	Queue       unsafe.Pointer // VkQueue
	QueueFamily uint32
	// Surface is the headless VkSurfaceKHR we created and passed to the core
	// during negotiation so it can create a swapchain.  We retain ownership;
	// the VulkanContext will destroy it in Deinit().
	Surface *HeadlessSurface
	// ExternalMemoryReady indicates whether the negotiated VkDevice was
	// created with VK_KHR_external_memory + VK_KHR_external_memory_fd
	// (injected by our create_device_wrapper).  When true, zero-copy Phase 3
	// is available even on the negotiated device path.
	ExternalMemoryReady bool
}

// NewVulkanContextFromNegotiation creates a VulkanContext that wraps handles
// provided by the core via the negotiation interface (e.g. Dolphin's device).
// The core owns the instance/device/queue; we only create a command pool.
//
// If r.ExternalMemoryReady is true (the create_device_wrapper successfully
// injected external-memory extensions), ExternalMemoryEnabled is set to true
// so the Phase 3 zero-copy path is available.
func NewVulkanContextFromNegotiation(cfg Config, r NegotiationResult) (*VulkanContext, error) {
	ctx, err := NewContextFromHandles(
		(C.VkInstance)(r.Instance),
		(C.VkPhysicalDevice)(r.PhysDevice),
		(C.VkDevice)(r.Device),
		(C.VkQueue)(r.Queue),
		r.QueueFamily,
	)
	if err != nil {
		return nil, fmt.Errorf("vulkan: NewContextFromHandles: %w", err)
	}

	// Propagate external-memory capability to the Context so that the
	// ZeroCopyBuffer allocation path and ZeroCopyFd export are enabled.
	ctx.ExternalMemoryEnabled = r.ExternalMemoryReady

	prov, err := NewProvider(ctx)
	if err != nil {
		ctx.Destroy()
		return nil, fmt.Errorf("vulkan: provider (negotiated): %w", err)
	}

	return &VulkanContext{
		cfg:      cfg,
		ctx:      ctx,
		provider: prov,
		surface:  r.Surface, // retain for cleanup in Deinit()
	}, nil
}

// ReadFramebuffer reads the current rendered frame from GPU memory and
// returns the raw RGBA pixel bytes.  size must equal w*h*4.
//
// Phase routing: if Phase 3 (zero-copy) is active this also triggers the
// GPU-to-GPU blit into the exportable buffer as a side effect, making the
// buffer available via ZeroCopyFd() immediately after this call returns.
func (v *VulkanContext) ReadFramebuffer(size, w, h uint) []byte {
	pixels, err := v.provider.ReadFrame(uint32(w), uint32(h))
	if err != nil {
		// Return an empty (black) frame on error rather than panicking.
		return make([]byte, size)
	}
	if uint(len(pixels)) > size {
		return pixels[:size]
	}
	return pixels
}

// ZeroCopyFd returns the Linux fd plus allocation size for the exportable
// Vulkan device memory after a ZeroCopy blit has been performed (i.e. after
// ReadFramebuffer or ReadFrameZeroCopy has been called this frame).
//
// Returns (-1, err) when Phase 3 is unavailable or no blit has happened yet.
// The CUDA layer uses this fd to import the memory without CPU involvement.
func (v *VulkanContext) ZeroCopyFd(w, h uint) (int, uint64, error) {
	return v.provider.ReadFrameZeroCopy(uint32(w), uint32(h))
}

// IsZeroCopyAvailable reports whether the Phase 3 GPU-only path is wired up
// on this device (VK_KHR_external_memory_fd present and ZeroCopyBuffer allocated).
func (v *VulkanContext) IsZeroCopyAvailable() bool {
	return v.ctx.ExternalMemoryEnabled
}

// WaitZeroCopyBlit waits for the most recent async BlitFrom to complete.
// Returns nil immediately if no blit is pending.
func (v *VulkanContext) WaitZeroCopyBlit() error {
	return v.provider.WaitZeroCopyBlit()
}

// RenderInterface returns the libretro Vulkan render interface pointer that
// the core should receive in response to RETRO_ENVIRONMENT_GET_HW_RENDER_INTERFACE.
// The return type is unsafe.Pointer so that callers in other packages (e.g.
// nanoarch) can write it into the void** data field without referencing a
// CGo type across package boundaries.
func (v *VulkanContext) RenderInterface() unsafe.Pointer {
	return unsafe.Pointer(v.provider.Interface())
}

// DestroyProvider unregisters the provider's cgo.Handle and releases its
// GPU resources (ZeroCopyBuffer, FrameCapture) WITHOUT destroying the
// underlying VkDevice.  Call this before context_destroy so that any
// libretro callbacks that fire during context_destroy (e.g. go_set_image)
// get a safe nil lookup instead of accessing freed memory.
func (v *VulkanContext) DestroyProvider() {
	if v.provider != nil {
		v.provider.Destroy()
	}
}

// Deinit destroys all Vulkan resources.
// DestroyProvider should be called before Deinit if context_destroy needs
// to be invoked in between (see deinitVulkanVideo in nanoarch_vulkan.go).
func (v *VulkanContext) Deinit() error {
	// Provider may already be destroyed by DestroyProvider; Destroy is safe
	// to call again because unregisterProvider guards with a handle==0 check
	// and the GPU resources are only freed once.
	v.provider.Destroy()
	// Destroy the headless surface before the instance it was created from.
	// surface.Destroy is nil-safe and instance-safe when surface is nil.
	if v.surface != nil {
		v.surface.Destroy(unsafe.Pointer(v.ctx.Instance))
		v.surface = nil
	}
	v.ctx.Destroy()
	return nil
}

// IsVulkan always returns true for this implementation; used by pipeline
// selection logic.
func (v *VulkanContext) IsVulkan() bool { return true }

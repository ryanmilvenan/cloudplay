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

// ZeroCopyFd returns the Linux fd for the exportable Vulkan device memory
// after a ZeroCopy blit has been performed (i.e. after ReadFramebuffer or
// ReadFrameZeroCopy has been called this frame).
//
// Returns (-1, err) when Phase 3 is unavailable or no blit has happened yet.
// The CUDA layer uses this fd to import the memory without CPU involvement.
func (v *VulkanContext) ZeroCopyFd(w, h uint) (int, error) {
	return v.provider.ReadFrameZeroCopy(uint32(w), uint32(h))
}

// IsZeroCopyAvailable reports whether the Phase 3 GPU-only path is wired up
// on this device (VK_KHR_external_memory_fd present and ZeroCopyBuffer allocated).
func (v *VulkanContext) IsZeroCopyAvailable() bool {
	return v.ctx.ExternalMemoryEnabled
}

// RenderInterface returns the libretro Vulkan render interface pointer that
// the core should receive in response to RETRO_ENVIRONMENT_GET_HW_RENDER_INTERFACE.
// The return type is unsafe.Pointer so that callers in other packages (e.g.
// nanoarch) can write it into the void** data field without referencing a
// CGo type across package boundaries.
func (v *VulkanContext) RenderInterface() unsafe.Pointer {
	return unsafe.Pointer(v.provider.Interface())
}

// Deinit destroys all Vulkan resources.
func (v *VulkanContext) Deinit() error {
	v.provider.Destroy()
	v.ctx.Destroy()
	return nil
}

// IsVulkan always returns true for this implementation; used by pipeline
// selection logic.
func (v *VulkanContext) IsVulkan() bool { return true }

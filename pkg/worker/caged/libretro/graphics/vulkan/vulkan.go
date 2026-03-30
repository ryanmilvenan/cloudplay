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

// RenderInterface returns the libretro Vulkan render interface pointer that
// the core should receive in response to RETRO_ENVIRONMENT_GET_HW_RENDER_INTERFACE.
func (v *VulkanContext) RenderInterface() *C.struct_retro_hw_render_interface_vulkan {
	return v.provider.Interface()
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

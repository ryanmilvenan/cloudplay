//go:build vulkan

package nanoarch

// Vulkan wiring for nanoarch: integrates the headless Vulkan context provider
// (pkg/worker/caged/libretro/graphics/vulkan) into the emulator loop.
//
// Design: all Vulkan-specific logic lives here; nanoarch.go delegates to
// these functions via thin call-sites guarded by `Nan0.Video.vulkan`.
// The GL path in nanoarch.go is completely unchanged.

/*
#include "libretro.h"
#include "nanoarch.h"
#include <stdlib.h>
*/
import "C"
import (
	"fmt"
	"unsafe"

	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/libretro/graphics/vulkan"
	"github.com/giongto35/cloud-game/v3/pkg/worker/thread"
)

// vulkanState holds all Vulkan-specific runtime state for Nanoarch.
// It is embedded in nanoarchVulkanExt which is wired into Nanoarch via
// the `vulkanExt` field added to the struct in this build.
type vulkanState struct {
	enabled bool
	ctx     *vulkan.VulkanContext
}

// ── Accessors ──────────────────────────────────────────────────────────────

// IsVulkan returns true when the core requested and received a Vulkan HW
// render context.
func (n *Nanoarch) IsVulkan() bool { return n.vulkan.enabled }

// ── Environment callback handlers ─────────────────────────────────────────

// handleVulkanHWRender is called from coreEnvironment when the core sends
// RETRO_ENVIRONMENT_SET_HW_RENDER with context_type == RETRO_HW_CONTEXT_VULKAN.
//
// At this point the Vulkan context hasn't been created yet (that happens in
// initVulkanVideo, triggered later by initVideo/LoadGame).  We just record
// that Vulkan was requested and clear the GL-specific callbacks so the core
// doesn't try to call glGetProcAddress.
//
// data is the raw unsafe.Pointer from coreEnvironment (points to
// struct retro_hw_render_callback).
func handleVulkanHWRender(data unsafe.Pointer) bool {
	hw := (*C.struct_retro_hw_render_callback)(data)
	Nan0.Video.hw = hw
	// Nullify GL-specific callbacks — Vulkan cores must not call them.
	hw.get_current_framebuffer = nil
	hw.get_proc_address = nil
	Nan0.vulkan.enabled = true
	// Vulkan does not need the main OS thread for rendering; disable main-
	// thread dispatch so thread.Main() calls run inline instead of queueing.
	thread.SwitchGraphics(false)
	Nan0.log.Info().Msg("Vulkan HW render requested by core")
	return true
}

// handleGetHWRenderInterface is called from coreEnvironment when the core sends
// RETRO_ENVIRONMENT_GET_HW_RENDER_INTERFACE.
//
// The core calls this after context_reset to obtain the
// retro_hw_render_interface_vulkan it needs to drive rendering.
// We write the interface pointer into the void** that data points to.
func handleGetHWRenderInterface(data unsafe.Pointer) bool {
	if !Nan0.vulkan.enabled || Nan0.vulkan.ctx == nil {
		Nan0.log.Warn().Msg("GET_HW_RENDER_INTERFACE called but Vulkan context is not ready")
		return false
	}
	iface := Nan0.vulkan.ctx.RenderInterface()
	if iface == nil {
		return false
	}
	// data points to a retro_hw_render_interface* (void*) that the core
	// has passed by address: *(void**)data = &retro_hw_render_interface_vulkan
	*(*unsafe.Pointer)(data) = iface
	return true
}

// ── Video lifecycle ───────────────────────────────────────────────────────

// initVulkanVideo creates the headless VulkanContext and immediately fires
// the core's context_reset callback so it can call GET_HW_RENDER_INTERFACE.
// This is the Vulkan equivalent of initVideo + context_reset for GL.
func initVulkanVideo() {
	w := uint32(Nan0.sys.av.geometry.max_width)
	h := uint32(Nan0.sys.av.geometry.max_height)

	ctx, err := vulkan.NewVulkanContext(vulkan.Config{Width: w, Height: h})
	if err != nil {
		panic(fmt.Sprintf("Vulkan: failed to create context: %v", err))
	}
	Nan0.vulkan.ctx = ctx
	Nan0.log.Info().Msgf("Vulkan context created (%dx%d)", w, h)

	// Fire context_reset so the core can call GET_HW_RENDER_INTERFACE.
	if Nan0.Video.hw != nil && Nan0.Video.hw.context_reset != nil {
		C.bridge_context_reset(Nan0.Video.hw.context_reset)
	}
}

// deinitVulkanVideo tears down the Vulkan context.  Safe to call even if the
// context was never initialised (e.g. core was never loaded).
func deinitVulkanVideo() {
	if Nan0.Video.hw != nil && !Nan0.hackSkipHwContextDestroy && Nan0.Video.hw.context_destroy != nil {
		C.bridge_context_reset(Nan0.Video.hw.context_destroy)
	}
	if Nan0.vulkan.ctx != nil {
		if err := Nan0.vulkan.ctx.Deinit(); err != nil {
			Nan0.log.Error().Err(err).Msg("Vulkan deinit error")
		}
		Nan0.vulkan.ctx = nil
	}
	Nan0.vulkan.enabled = false
	Nan0.hackSkipHwContextDestroy = false
}

// ── Frame readback ────────────────────────────────────────────────────────

// readVulkanFramebuffer reads the current rendered frame from Vulkan GPU
// memory and returns the raw pixel bytes.
// Called from coreVideoRefresh when data == RETRO_HW_FRAME_BUFFER_VALID and
// Vulkan is active.
//
// Phase 3 behaviour: if the device supports VK_KHR_external_memory_fd, this
// call also performs the GPU-to-GPU blit into the exportable ZeroCopyBuffer.
// nanoarch can then call vulkanZeroCopyFd() to retrieve the fd for CUDA import
// without going through the CPU-side []byte path.
func readVulkanFramebuffer(size, w, h uint) []byte {
	if Nan0.vulkan.ctx == nil {
		return make([]byte, size)
	}
	return Nan0.vulkan.ctx.ReadFramebuffer(size, w, h)
}

// vulkanZeroCopyFd returns the Linux fd for the current frame's exportable
// Vulkan device memory after readVulkanFramebuffer has been called.
//
// Returns (-1, err) when Phase 3 is not available.
// Used by the media pipeline to drive NVENC via CUDA without CPU copies.
func vulkanZeroCopyFd(w, h uint) (int, error) {
	if Nan0.vulkan.ctx == nil {
		return -1, fmt.Errorf("vulkan: context not initialised")
	}
	return Nan0.vulkan.ctx.ZeroCopyFd(w, h)
}

// IsZeroCopyAvailable reports whether Phase 3 GPU-only encoding is available
// for this session (device supports VK_KHR_external_memory_fd).
func (n *Nanoarch) IsZeroCopyAvailable() bool {
	if n.vulkan.ctx == nil {
		return false
	}
	return n.vulkan.ctx.IsZeroCopyAvailable()
}

// ZeroCopyFd returns the Linux fd for the exportable Vulkan device memory
// of the current frame.  Delegates to vulkanZeroCopyFd.
func (n *Nanoarch) ZeroCopyFd(w, h uint) (int, error) {
	return vulkanZeroCopyFd(w, h)
}

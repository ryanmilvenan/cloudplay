//go:build vulkan

package nanoarch

// Vulkan wiring for nanoarch: integrates the headless Vulkan context provider
// (pkg/worker/caged/libretro/graphics/vulkan) into the emulator loop.
//
// Design: all Vulkan-specific logic lives here; nanoarch.go delegates to
// these functions via thin call-sites guarded by `Nan0.Video.vulkan`.
// The GL path in nanoarch.go is completely unchanged.

/*
#cgo LDFLAGS: -lvulkan
#include "libretro.h"
#include "nanoarch.h"
#include "../graphics/vulkan/libretro_vulkan.h"
#include <stdlib.h>
#include <string.h>

// Trampoline: call create_device2 from the core's negotiation interface.
// Returns true on success, filling in ctx.
// If create_device2 is NULL, falls back to create_device (v1).
// Uses vkGetInstanceProcAddr from the Vulkan loader directly.
static bool call_create_device(
    const struct retro_hw_render_context_negotiation_interface_vulkan *neg,
    struct retro_vulkan_context *ctx,
    VkInstance instance,
    VkPhysicalDevice gpu)
{
    if (!neg) return false;

    // v2: prefer create_device2 when interface_version >= 2
    if (neg->interface_version >= 2 && neg->create_device2) {
        return neg->create_device2(ctx, instance, gpu, VK_NULL_HANDLE,
                                   vkGetInstanceProcAddr,
                                   NULL, // create_device_wrapper: use default
                                   NULL); // opaque
    }
    // v1 fallback
    if (neg->create_device) {
        return neg->create_device(ctx, instance, gpu, VK_NULL_HANDLE,
                                  vkGetInstanceProcAddr,
                                  NULL, 0, // no required extensions
                                  NULL, 0, // no required layers
                                  NULL);   // no required features
    }
    return false;
}
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
	// negotiationIface is the core's negotiation interface, set when
	// the core calls RETRO_ENVIRONMENT_SET_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE.
	// We use it in initVulkanVideo to let the core create the VkDevice.
	negotiationIface unsafe.Pointer
}

// ── Accessors ──────────────────────────────────────────────────────────────

// IsVulkan returns true when the core requested and received a Vulkan HW
// render context.
func (n *Nanoarch) IsVulkan() bool { return n.vulkan.enabled }

// preferredHWContextIsVulkan tells the core environment callback to advertise
// Vulkan as the preferred hardware render path on Vulkan-enabled builds.
func preferredHWContextIsVulkan() bool { return true }

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
//
// If the core provided a negotiation interface via
// SET_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE (e.g. Dolphin does this), we
// use the negotiation path: create a minimal instance+GPU ourselves, then let
// the core create the VkDevice via create_device2 with the extensions it needs.
// This ensures Dolphin's dispatch table has all required function pointers.
//
// If no negotiation interface is available, fall back to creating the full
// device ourselves (original Phase 2 behaviour).
func initVulkanVideo() {
	w := uint32(Nan0.sys.av.geometry.max_width)
	h := uint32(Nan0.sys.av.geometry.max_height)
	cfg := vulkan.Config{Width: w, Height: h}

	var ctx *vulkan.VulkanContext
	var err error

	if Nan0.vulkan.negotiationIface != nil {
		// Negotiation path: let the core create the VkDevice.
		Nan0.log.Info().Msg("Vulkan: using core negotiation to create device")
		ctx, err = initVulkanVideoWithNegotiation(cfg)
		if err != nil {
			// Fall back to our own device creation on negotiation failure.
			Nan0.log.Warn().Err(err).Msg("Vulkan negotiation failed, falling back to frontend device")
			ctx, err = vulkan.NewVulkanContext(cfg)
		}
	} else {
		ctx, err = vulkan.NewVulkanContext(cfg)
	}
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

// initVulkanVideoWithNegotiation creates a VulkanContext by delegating device
// creation to the core via its negotiation interface callbacks.
func initVulkanVideoWithNegotiation(cfg vulkan.Config) (*vulkan.VulkanContext, error) {
	// Create a minimal instance + GPU selection (no VkDevice yet).
	baseCtx, err := vulkan.NewInstanceOnly()
	if err != nil {
		return nil, fmt.Errorf("negotiation: failed to create instance: %w", err)
	}

	// Call the core's create_device2 (or create_device) so it creates the
	// VkDevice with all the extensions it needs.
	neg := (*C.struct_retro_hw_render_context_negotiation_interface_vulkan)(Nan0.vulkan.negotiationIface)
	var vkCtx C.struct_retro_vulkan_context
	// Pass instance and physDevice via unsafe.Pointer to avoid cross-package
	// C type issues; the C helper casts them correctly.
	ok := C.call_create_device(
		neg,
		&vkCtx,
		(C.VkInstance)(baseCtx.VkInstancePtr()),
		(C.VkPhysicalDevice)(baseCtx.VkPhysDevicePtr()),
	)
	if !bool(ok) {
		baseCtx.DestroyInstanceOnly()
		return nil, fmt.Errorf("negotiation: create_device returned false")
	}

	Nan0.log.Info().Msgf("Vulkan negotiation: device created by core, queue_family=%d", uint32(vkCtx.queue_family_index))

	// Use the device/queue the core created.
	result := vulkan.NegotiationResult{
		Instance:    baseCtx.VkInstancePtr(),
		PhysDevice:  unsafe.Pointer(vkCtx.gpu),
		Device:      unsafe.Pointer(vkCtx.device),
		Queue:       unsafe.Pointer(vkCtx.queue),
		QueueFamily: uint32(vkCtx.queue_family_index),
	}
	return vulkan.NewVulkanContextFromNegotiation(cfg, result)
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

// ── Context negotiation handlers ──────────────────────────────────────────

// handleSetHWRenderContextNegotiation handles
// RETRO_ENVIRONMENT_SET_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE (43).
//
// Dolphin calls this to provide a negotiation interface struct with callbacks
// for device creation (create_device / create_device2).  At this stage our
// VulkanContext hasn't been created yet (that happens in initVulkanVideo via
// LoadGame), so we just store the pointer.
//
// The actual negotiation (calling create_device2 to let Dolphin create the
// VkDevice with the extensions it needs) is not yet implemented — we use our
// own pre-created device.  Returning true here tells the core "negotiation is
// supported", which satisfies Dolphin's check and prevents an early bail-out.
//
// If the returned interface_version mismatch causes further issues, the next
// step would be to call negotiationIface.create_device2 from initVulkanVideo
// so Dolphin can create the VkDevice itself.
func handleSetHWRenderContextNegotiation(data unsafe.Pointer) bool {
	if data == nil {
		return false
	}
	// Store the pointer so initVulkanVideo can call create_device2/create_device.
	Nan0.vulkan.negotiationIface = data
	Nan0.log.Info().Msg("SET_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE: stored core negotiation callbacks")
	return true
}

// handleGetHWRenderContextNegotiationSupport handles
// RETRO_ENVIRONMENT_GET_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_SUPPORT (73).
//
// The core calls this before SET_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE to
// ask which negotiation interface version the frontend supports.  We report
// version 2 (RETRO_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_VULKAN_VERSION).
//
// data points to struct retro_hw_render_interface with fields:
//   interface_type    (int32, set by core on input)
//   interface_version (uint32, set by frontend on output)
func handleGetHWRenderContextNegotiationSupport(data unsafe.Pointer) bool {
	if data == nil {
		return true // returning true indicates the call is supported
	}
	// struct retro_hw_render_interface: { int interface_type; unsigned interface_version; }
	// Use *[2]uint32 to access both fields as raw memory.
	fields := (*[2]uint32)(data)
	// fields[0] = interface_type (read-only, set by core)
	// fields[1] = interface_version (write, set by frontend to max we support)
	//
	// Report max negotiation interface version we acknowledge.
	// 2 = RETRO_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_VULKAN_VERSION
	// (we acknowledge v2 protocol but use our own pre-built device — Dolphin
	// will negotiate at v1 level since we don't call create_device2 yet).
	fields[1] = 2
	Nan0.log.Debug().Msgf("GET_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_SUPPORT: type=%d, reporting max version 2",
		int(fields[0]))
	return true
}

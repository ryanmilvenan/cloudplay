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
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// ── External-memory extension injection ───────────────────────────────────
//
// When Dolphin calls create_device2 with our create_device_wrapper, it passes
// us the VkDeviceCreateInfo it has built.  We append VK_KHR_external_memory
// and VK_KHR_external_memory_fd to ppEnabledExtensionNames so the resulting
// VkDevice supports the zero-copy export path.
//
// If either extension is unavailable the vkCreateDevice call returns
// VK_ERROR_EXTENSION_NOT_PRESENT; in that case we retry without them and
// set g_extmem_injected = false so the caller knows to skip zero-copy.

static int g_extmem_injected = 0; // 1 if extensions were successfully injected

static const char *kExternalMemExts[] = {
    "VK_KHR_external_memory",
    "VK_KHR_external_memory_fd",
};
#define N_EXTMEM_EXTS 2

static const char *kSurfaceExts[] = {
    "VK_KHR_surface",
    "VK_EXT_headless_surface",
    "VK_KHR_xlib_surface",
};
#define N_SURFACE_EXTS 3

static int has_ext(const char *const *exts, uint32_t count, const char *want)
{
    if (!exts || !want) return 0;
    for (uint32_t i = 0; i < count; i++) {
        if (exts[i] && strcmp(exts[i], want) == 0) return 1;
    }
    return 0;
}

static int should_block_device_ext_diag(const char *name)
{
    if (!name) return 0;
    return strcmp(name, "VK_NV_low_latency2") == 0 ||
           strcmp(name, "VK_EXT_mesh_shader") == 0 ||
           strcmp(name, "VK_EXT_device_generated_commands") == 0 ||
           strcmp(name, "VK_KHR_fragment_shader_barycentric") == 0 ||
           strcmp(name, "VK_EXT_external_memory_host") == 0 ||
           strcmp(name, "VK_EXT_pageable_device_local_memory") == 0 ||
           strcmp(name, "VK_NV_descriptor_pool_overallocation") == 0 ||
           strcmp(name, "VK_EXT_memory_priority") == 0 ||
           strcmp(name, "VK_EXT_memory_budget") == 0;
}

static void dump_device_create_info(const char *tag, const VkDeviceCreateInfo *info)
{
    if (!tag || !info) return;
    fprintf(stderr,
            "[cloudplay/vk-neg] %s: flags=%u queue_infos=%u layers=%u exts=%u pNext=%p features=%p\n",
            tag,
            (unsigned)info->flags,
            (unsigned)info->queueCreateInfoCount,
            (unsigned)info->enabledLayerCount,
            (unsigned)info->enabledExtensionCount,
            info->pNext,
            info->pEnabledFeatures);
    fflush(stderr);

    for (uint32_t i = 0; i < info->enabledExtensionCount; i++) {
        fprintf(stderr,
                "[cloudplay/vk-neg] %s: ext[%u]=%s\n",
                tag, i,
                info->ppEnabledExtensionNames && info->ppEnabledExtensionNames[i]
                    ? info->ppEnabledExtensionNames[i]
                    : "<null>");
    }
    fflush(stderr);

    if (info->pEnabledFeatures) {
        const VkPhysicalDeviceFeatures *f = info->pEnabledFeatures;
        fprintf(stderr,
                "[cloudplay/vk-neg] %s: features robustBufferAccess=%u fullDrawIndexUint32=%u imageCubeArray=%u geometryShader=%u tessellationShader=%u samplerAnisotropy=%u multiViewport=%u shaderInt16=%u fragmentStoresAndAtomics=%u vertexPipelineStoresAndAtomics=%u\n",
                tag,
                (unsigned)f->robustBufferAccess,
                (unsigned)f->fullDrawIndexUint32,
                (unsigned)f->imageCubeArray,
                (unsigned)f->geometryShader,
                (unsigned)f->tessellationShader,
                (unsigned)f->samplerAnisotropy,
                (unsigned)f->multiViewport,
                (unsigned)f->shaderInt16,
                (unsigned)f->fragmentStoresAndAtomics,
                (unsigned)f->vertexPipelineStoresAndAtomics);
        fflush(stderr);
    }

    const VkBaseInStructure *node = (const VkBaseInStructure *)info->pNext;
    uint32_t idx = 0;
    while (node && idx < 32) {
        fprintf(stderr,
                "[cloudplay/vk-neg] %s: pNext[%u].sType=%u ptr=%p next=%p\n",
                tag, idx, (unsigned)node->sType, (const void *)node, node->pNext);
        node = node->pNext;
        idx++;
    }
    if (idx == 0) {
        fprintf(stderr, "[cloudplay/vk-neg] %s: pNext chain empty\n", tag);
        fflush(stderr);
    }
}

// create_instance_with_surface_exts is the VkInstance-wrapper callback passed
// to v2 create_instance. It preserves the core's requested instance create
// info while ensuring the surface extensions we need for headless swapchain
// creation are present.
static VkInstance create_instance_with_surface_exts(
    void *opaque,
    const VkInstanceCreateInfo *core_info)
{
    (void)opaque;
    if (!core_info) return VK_NULL_HANDLE;

    uint32_t avail_count = 0;
    vkEnumerateInstanceExtensionProperties(NULL, &avail_count, NULL);
    VkExtensionProperties *avail = NULL;
    if (avail_count > 0) {
        avail = (VkExtensionProperties *)malloc(avail_count * sizeof(VkExtensionProperties));
        if (!avail) return VK_NULL_HANDLE;
        vkEnumerateInstanceExtensionProperties(NULL, &avail_count, avail);
    }

    uint32_t extra = 0;
    for (int i = 0; i < N_SURFACE_EXTS; i++) {
        int supported = 0;
        for (uint32_t ai = 0; ai < avail_count; ai++) {
            if (strcmp(avail[ai].extensionName, kSurfaceExts[i]) == 0) {
                supported = 1;
                break;
            }
        }
        if (supported && !has_ext(core_info->ppEnabledExtensionNames, core_info->enabledExtensionCount, kSurfaceExts[i])) {
            extra++;
        }
    }

    uint32_t total = core_info->enabledExtensionCount + extra;
    const char **exts = NULL;
    if (total > 0) {
        exts = (const char **)malloc(total * sizeof(const char *));
        if (!exts) {
            if (avail) free(avail);
            return VK_NULL_HANDLE;
        }
    }

    uint32_t idx = 0;
    for (uint32_t i = 0; i < core_info->enabledExtensionCount; i++) {
        exts[idx++] = core_info->ppEnabledExtensionNames[i];
    }
    for (int i = 0; i < N_SURFACE_EXTS; i++) {
        int supported = 0;
        for (uint32_t ai = 0; ai < avail_count; ai++) {
            if (strcmp(avail[ai].extensionName, kSurfaceExts[i]) == 0) {
                supported = 1;
                break;
            }
        }
        if (supported && !has_ext(core_info->ppEnabledExtensionNames, core_info->enabledExtensionCount, kSurfaceExts[i])) {
            exts[idx++] = kSurfaceExts[i];
        }
    }

    VkInstanceCreateInfo info = *core_info;
    info.enabledExtensionCount = total;
    info.ppEnabledExtensionNames = total > 0 ? exts : NULL;

    fprintf(stderr,
            "[cloudplay/vk-neg] create_instance wrapper: core_exts=%u added_surface_exts=%u api=%u\n",
            core_info->enabledExtensionCount, extra,
            core_info->pApplicationInfo ? core_info->pApplicationInfo->apiVersion : 0);
    fflush(stderr);

    VkInstance instance = VK_NULL_HANDLE;
    VkResult res = vkCreateInstance(&info, NULL, &instance);
    fprintf(stderr,
            "[cloudplay/vk-neg] create_instance wrapper: vkCreateInstance res=%d instance=%p\n",
            (int)res, (void *)instance);
    fflush(stderr);

    if (exts) free(exts);
    if (avail) free(avail);
    return (res == VK_SUCCESS) ? instance : VK_NULL_HANDLE;
}

static VkDevice create_device_passthrough(
    VkPhysicalDevice gpu,
    void *opaque,
    const VkDeviceCreateInfo *core_info)
{
    (void)opaque;
    if (!core_info) return VK_NULL_HANDLE;
    fprintf(stderr,
            "[cloudplay/vk-neg] create_device passthrough: entered core_exts=%u\n",
            core_info->enabledExtensionCount);
    fflush(stderr);
    dump_device_create_info("create_device_passthrough.core_info", core_info);

    uint32_t kept = 0;
    for (uint32_t i = 0; i < core_info->enabledExtensionCount; i++) {
        const char *name = core_info->ppEnabledExtensionNames ? core_info->ppEnabledExtensionNames[i] : NULL;
        if (!should_block_device_ext_diag(name)) kept++;
    }

    const char **exts = NULL;
    if (kept > 0) {
        exts = (const char **)malloc(kept * sizeof(const char *));
        if (!exts) return VK_NULL_HANDLE;
    }
    int core_has_extmem_fd = 0;
    uint32_t idx = 0;
    for (uint32_t i = 0; i < core_info->enabledExtensionCount; i++) {
        const char *name = core_info->ppEnabledExtensionNames ? core_info->ppEnabledExtensionNames[i] : NULL;
        if (should_block_device_ext_diag(name)) {
            fprintf(stderr, "[cloudplay/vk-neg] create_device passthrough: diag-blocking ext=%s\n", name ? name : "<null>");
            fflush(stderr);
            continue;
        }
        if (name && strcmp(name, "VK_KHR_external_memory_fd") == 0) {
            core_has_extmem_fd = 1;
        }
        if (exts) exts[idx++] = name;
    }

    VkDeviceCreateInfo info = *core_info;
    // Diagnostic follow-up (2026-04-03): preserve the core's requested
    // feature chain in the passthrough path. Stripping pNext may allow device
    // creation to succeed while silently removing features LRPS2 expects to
    // exist during context_reset.
    info.pNext = core_info->pNext;
    info.enabledExtensionCount = kept;
    info.ppEnabledExtensionNames = exts;
    dump_device_create_info("create_device_passthrough.stripped_info", &info);
    VkDevice device = VK_NULL_HANDLE;
    VkResult res = vkCreateDevice(gpu, &info, NULL, &device);
    fprintf(stderr,
            "[cloudplay/vk-neg] create_device passthrough: vkCreateDevice(core-only) res=%d device=%p\n",
            (int)res, (void *)device);
    fflush(stderr);

    // Diagnostic compromise: if the core's sanitized request already includes
    // the export-critical FD extension, treat external memory as available
    // without mutating device creation. On Vulkan 1.1+ the base external-memory
    // capability is core, so injecting VK_KHR_external_memory itself may be
    // unnecessary for this path.
    g_extmem_injected = core_has_extmem_fd ? 1 : 0;
    fprintf(stderr,
            "[cloudplay/vk-neg] create_device passthrough: extmem_ready_from_core=%d\n",
            g_extmem_injected);
    fflush(stderr);

    if (exts) free(exts);
    return (res == VK_SUCCESS) ? device : VK_NULL_HANDLE;
}

// create_device_with_extmem is the VkDevice-wrapper callback passed to
// create_device2.  It injects external-memory extensions into the core's
// VkDeviceCreateInfo before calling vkCreateDevice.
static VkDevice create_device_with_extmem(
    VkPhysicalDevice gpu,
    void *opaque,
    const VkDeviceCreateInfo *core_info)
{
    (void)opaque;
    if (!core_info) return VK_NULL_HANDLE;

    uint32_t avail_count = 0;
    vkEnumerateDeviceExtensionProperties(gpu, NULL, &avail_count, NULL);
    VkExtensionProperties *avail = NULL;
    if (avail_count > 0) {
        avail = (VkExtensionProperties *)malloc(avail_count * sizeof(VkExtensionProperties));
        if (!avail) return VK_NULL_HANDLE;
        vkEnumerateDeviceExtensionProperties(gpu, NULL, &avail_count, avail);
    }

    uint32_t kept = 0;
    for (uint32_t i = 0; i < core_info->enabledExtensionCount; i++) {
        const char *name = core_info->ppEnabledExtensionNames ? core_info->ppEnabledExtensionNames[i] : NULL;
        if (!should_block_device_ext_diag(name)) kept++;
    }

    uint32_t extra = 0;
    for (int i = 0; i < N_EXTMEM_EXTS; i++) {
        int supported = 0;
        for (uint32_t ai = 0; ai < avail_count; ai++) {
            if (strcmp(avail[ai].extensionName, kExternalMemExts[i]) == 0) {
                supported = 1;
                break;
            }
        }
        if (supported && !has_ext(core_info->ppEnabledExtensionNames, core_info->enabledExtensionCount, kExternalMemExts[i])) {
            extra++;
        }
    }

    uint32_t total = kept + extra;
    const char **exts = NULL;
    if (total > 0) {
        exts = (const char **)malloc(total * sizeof(const char *));
        if (!exts) {
            if (avail) free(avail);
            return VK_NULL_HANDLE;
        }
    }

    uint32_t idx = 0;
    for (uint32_t i = 0; i < core_info->enabledExtensionCount; i++) {
        const char *name = core_info->ppEnabledExtensionNames ? core_info->ppEnabledExtensionNames[i] : NULL;
        if (should_block_device_ext_diag(name)) {
            fprintf(stderr, "[cloudplay/vk-neg] create_device wrapper: diag-blocking ext=%s\n", name ? name : "<null>");
            fflush(stderr);
            continue;
        }
        if (exts) exts[idx++] = name;
    }
    for (int i = 0; i < N_EXTMEM_EXTS; i++) {
        int supported = 0;
        for (uint32_t ai = 0; ai < avail_count; ai++) {
            if (strcmp(avail[ai].extensionName, kExternalMemExts[i]) == 0) {
                supported = 1;
                break;
            }
        }
        if (supported && !has_ext(core_info->ppEnabledExtensionNames, core_info->enabledExtensionCount, kExternalMemExts[i])) {
            if (exts) exts[idx++] = kExternalMemExts[i];
        }
    }

    VkDeviceCreateInfo info = *core_info;
    info.pNext = NULL; // keep the stable diagnostic baseline
    info.enabledExtensionCount   = total;
    info.ppEnabledExtensionNames = total > 0 ? exts : NULL;

    fprintf(stderr,
            "[cloudplay/vk-neg] create_device wrapper: kept_core_exts=%u added_extmem_exts=%u\n",
            kept, extra);
    fflush(stderr);
    dump_device_create_info("create_device_with_extmem.core_info", core_info);
    dump_device_create_info("create_device_with_extmem.final_info", &info);

    VkDevice device = VK_NULL_HANDLE;
    VkResult res = vkCreateDevice(gpu, &info, NULL, &device);
    fprintf(stderr,
            "[cloudplay/vk-neg] create_device wrapper: vkCreateDevice(+extmem) res=%d device=%p\n",
            (int)res, (void *)device);
    fflush(stderr);

    if (exts) free(exts);
    if (avail) free(avail);

    if (res == VK_SUCCESS) {
        g_extmem_injected = 1;
        return device;
    }

    g_extmem_injected = 0;
    return VK_NULL_HANDLE;
}

// Trampoline: call create_device2 from the core's negotiation interface.
// Returns true on success, filling in ctx.
// If create_device2 is NULL, falls back to create_device (v1).
// Uses vkGetInstanceProcAddr from the Vulkan loader directly.
//
// Phase 3D: passes create_device_with_extmem as the device-creation wrapper
// so that external-memory extensions are injected into the negotiated VkDevice,
// enabling the zero-copy GPU→CUDA→NVENC path.
//
// surface: headless VkSurfaceKHR to pass to the core so it can create a
//   swapchain and begin calling retro_video_refresh().  Must not be
//   VK_NULL_HANDLE — cores like Dolphin, Flycast and PPSSPP skip swapchain
//   creation (and therefore never call retro_video_refresh) when this is NULL.
// ── Headless surface query stubs ──────────────────────────────────────────
// When using VK_EXT_headless_surface or Xvfb-backed xlib surfaces, the NVIDIA
// driver may fail surface queries with VK_ERROR_UNKNOWN (-13).  Dolphin's
// VulkanLoader.cpp resolves these via vkGetInstanceProcAddr at device creation
// time, so we intercept the proc addr function we pass to create_device2.

static VKAPI_ATTR VkResult VKAPI_CALL stub_GetPhysicalDeviceSurfaceFormatsKHR(
    VkPhysicalDevice gpu, VkSurfaceKHR surface,
    uint32_t *count, VkSurfaceFormatKHR *formats)
{
    VkResult r = vkGetPhysicalDeviceSurfaceFormatsKHR(gpu, surface, count, formats);
    if (r == VK_SUCCESS) return r;
    if (!formats) { *count = 1; return VK_SUCCESS; }
    if (*count >= 1) {
        formats[0].format     = VK_FORMAT_B8G8R8A8_UNORM;
        formats[0].colorSpace = VK_COLOR_SPACE_SRGB_NONLINEAR_KHR;
        *count = 1;
    }
    return VK_SUCCESS;
}

static VKAPI_ATTR VkResult VKAPI_CALL stub_GetPhysicalDeviceSurfacePresentModesKHR(
    VkPhysicalDevice gpu, VkSurfaceKHR surface,
    uint32_t *count, VkPresentModeKHR *modes)
{
    VkResult r = vkGetPhysicalDeviceSurfacePresentModesKHR(gpu, surface, count, modes);
    if (r == VK_SUCCESS) return r;
    if (!modes) { *count = 1; return VK_SUCCESS; }
    if (*count >= 1) { modes[0] = VK_PRESENT_MODE_FIFO_KHR; *count = 1; }
    return VK_SUCCESS;
}

static VKAPI_ATTR VkResult VKAPI_CALL stub_GetPhysicalDeviceSurfaceCapabilitiesKHR(
    VkPhysicalDevice gpu, VkSurfaceKHR surface,
    VkSurfaceCapabilitiesKHR *caps)
{
    // ALWAYS override capabilities, even when the real driver succeeds.
    // The real driver may return currentExtent={1,1} for headless/Xvfb surfaces,
    // causing Dolphin to create 1×1 swapchain images (which was the root cause
    // of the DEVICE_LOST: we tried to copy 640×528 from a 1×1 image).
    //
    // Try the real driver first to get baseline values, then override extent.
    VkResult r = vkGetPhysicalDeviceSurfaceCapabilitiesKHR(gpu, surface, caps);
    if (r != VK_SUCCESS) {
        // Driver failed — fill everything from scratch.
        memset(caps, 0, sizeof(*caps));
        caps->maxImageArrayLayers = 1;
        caps->supportedTransforms = VK_SURFACE_TRANSFORM_IDENTITY_BIT_KHR;
        caps->currentTransform    = VK_SURFACE_TRANSFORM_IDENTITY_BIT_KHR;
        caps->supportedCompositeAlpha = VK_COMPOSITE_ALPHA_OPAQUE_BIT_KHR;
    }
    // Always override extent and image count to ensure Dolphin creates
    // full-resolution swapchain images.
    caps->minImageCount = 2;
    caps->maxImageCount = 8;
    caps->currentExtent.width = 640;  caps->currentExtent.height = 528;
    caps->minImageExtent.width = 1;   caps->minImageExtent.height = 1;
    caps->maxImageExtent.width = 4096; caps->maxImageExtent.height = 4096;
    caps->supportedUsageFlags = VK_IMAGE_USAGE_COLOR_ATTACHMENT_BIT |
        VK_IMAGE_USAGE_TRANSFER_SRC_BIT | VK_IMAGE_USAGE_TRANSFER_DST_BIT |
        VK_IMAGE_USAGE_SAMPLED_BIT;
    return VK_SUCCESS;
}

static VKAPI_ATTR VkBool32 VKAPI_CALL stub_GetPhysicalDeviceSurfaceSupportKHR_inner(
    VkPhysicalDevice gpu, uint32_t qf, VkSurfaceKHR surface, VkBool32 *supported)
{
    VkResult r = vkGetPhysicalDeviceSurfaceSupportKHR(gpu, qf, surface, supported);
    if (r == VK_SUCCESS) return r;
    *supported = VK_TRUE;
    return VK_SUCCESS;
}

// Intercepting vkGetInstanceProcAddr: return stubs for surface queries.
static VKAPI_ATTR PFN_vkVoidFunction VKAPI_CALL cloudplay_intercepting_GetInstanceProcAddr(
    VkInstance instance, const char *name)
{
    if (strcmp(name, "vkGetPhysicalDeviceSurfaceFormatsKHR") == 0)
        return (PFN_vkVoidFunction)stub_GetPhysicalDeviceSurfaceFormatsKHR;
    if (strcmp(name, "vkGetPhysicalDeviceSurfacePresentModesKHR") == 0)
        return (PFN_vkVoidFunction)stub_GetPhysicalDeviceSurfacePresentModesKHR;
    if (strcmp(name, "vkGetPhysicalDeviceSurfaceCapabilitiesKHR") == 0)
        return (PFN_vkVoidFunction)stub_GetPhysicalDeviceSurfaceCapabilitiesKHR;
    if (strcmp(name, "vkGetPhysicalDeviceSurfaceSupportKHR") == 0)
        return (PFN_vkVoidFunction)stub_GetPhysicalDeviceSurfaceSupportKHR_inner;
    return vkGetInstanceProcAddr(instance, name);
}

static VkInstance call_create_instance(
    const struct retro_hw_render_context_negotiation_interface_vulkan *neg)
{
    if (!neg || neg->interface_version < 2 || !neg->create_instance) {
        fprintf(stderr, "[cloudplay/vk-neg] call_create_instance: v2 create_instance unavailable\n");
        fflush(stderr);
        return VK_NULL_HANDLE;
    }
    const VkApplicationInfo *app = NULL;
    if (neg->get_application_info) {
        app = neg->get_application_info();
    }
    VkInstance instance = neg->create_instance(
        cloudplay_intercepting_GetInstanceProcAddr,
        app,
        create_instance_with_surface_exts,
        NULL);
    fprintf(stderr,
            "[cloudplay/vk-neg] create_instance returned instance=%p app=%p\n",
            (void *)instance, (void *)app);
    fflush(stderr);
    return instance;
}

static bool call_create_device(
    const struct retro_hw_render_context_negotiation_interface_vulkan *neg,
    struct retro_vulkan_context *ctx,
    VkInstance instance,
    VkPhysicalDevice gpu,
    VkSurfaceKHR surface)
{
    if (!neg) {
        fprintf(stderr, "[cloudplay/vk-neg] call_create_device: neg=NULL\n");
        fflush(stderr);
        return false;
    }
    g_extmem_injected = 0;

    fprintf(stderr,
            "[cloudplay/vk-neg] call_create_device: iface_version=%u create_device=%p create_device2=%p instance=%p gpu=%p surface=%p\n",
            neg->interface_version, (void *)neg->create_device, (void *)neg->create_device2,
            (void *)instance, (void *)gpu, (void *)surface);
    fflush(stderr);

    // v2: prefer create_device2 when interface_version >= 2.
    // If it fails and the core also exposes a v1 callback, retry the older
    // path before falling back to a frontend-owned device. LRPS2 currently
    // exposes both, and we want proof about whether the failure is specific to
    // the v2 negotiation path.
    if (neg->interface_version >= 2 && neg->create_device2) {
        bool ok = neg->create_device2(ctx, instance, gpu, surface,
                                      cloudplay_intercepting_GetInstanceProcAddr,
                                      create_device_passthrough,
                                      NULL); // opaque
        fprintf(stderr,
                "[cloudplay/vk-neg] create_device2 returned ok=%d device=%p queue=%p qf=%u extmem=%d\n",
                ok ? 1 : 0, (void *)ctx->device, (void *)ctx->queue,
                ctx->queue_family_index, g_extmem_injected);
        fflush(stderr);
        if (ok) return true;
        fprintf(stderr, "[cloudplay/vk-neg] create_device2 failed; retrying v1 create_device path if available\n");
        fflush(stderr);
    }
    // v1 fallback: pass external-memory as required extensions.
    if (neg->create_device) {
        bool ok = neg->create_device(ctx, instance, gpu, surface,
                                     cloudplay_intercepting_GetInstanceProcAddr,
                                     (const char **)kExternalMemExts, N_EXTMEM_EXTS,
                                     NULL, 0, NULL);
        fprintf(stderr,
                "[cloudplay/vk-neg] create_device(v1,+extmem) returned ok=%d device=%p queue=%p qf=%u\n",
                ok ? 1 : 0, (void *)ctx->device, (void *)ctx->queue,
                ctx->queue_family_index);
        fflush(stderr);
        if (ok) {
            g_extmem_injected = 1;
            return true;
        }
        g_extmem_injected = 0;
        ok = neg->create_device(ctx, instance, gpu, surface,
                                cloudplay_intercepting_GetInstanceProcAddr,
                                NULL, 0, NULL, 0, NULL);
        fprintf(stderr,
                "[cloudplay/vk-neg] create_device(v1,no-extmem) returned ok=%d device=%p queue=%p qf=%u\n",
                ok ? 1 : 0, (void *)ctx->device, (void *)ctx->queue,
                ctx->queue_family_index);
        fflush(stderr);
        return ok;
    }
    fprintf(stderr, "[cloudplay/vk-neg] call_create_device: no usable callbacks\n");
    fflush(stderr);
    return false;
}

// extmem_was_injected returns 1 if the last call_create_device successfully
// injected external-memory extensions into the negotiated VkDevice.
static int extmem_was_injected(void) { return g_extmem_injected; }
*/
import "C"
import (
	"fmt"
	"strings"
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

// isGLFallbackCore returns true for cores that should be forced onto the
// OpenGL path because their Vulkan startup crashes in our frontend or because
// the core rejects our current Vulkan negotiation path.
//
// Current known fallback cores:
// - Flycast
//
// LRPS2 / PCSX2 is intentionally NOT hard-blocked here right now because the
// active work is specifically to make the Vulkan negotiation path succeed.
func isGLFallbackCore() bool {
	lib := strings.ToLower(Nan0.meta.LibPath)
	return strings.Contains(lib, "flycast") || strings.Contains(lib, "play_")
}

// preferredHWContextIsVulkan tells the core environment callback whether to
// advertise Vulkan as the preferred hardware render path.
//
// Cores that crash during Vulkan startup or reject our Vulkan negotiation path
// (currently Flycast and LRPS2/PCSX2) are sent to the GL fallback path instead.
func preferredHWContextIsVulkan() bool {
	if isGLFallbackCore() {
		Nan0.log.Info().Msgf("Vulkan: not advertising Vulkan as preferred API for GL-fallback core (%s)", Nan0.meta.LibPath)
		return false
	}
	return true
}

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
	if isGLFallbackCore() {
		Nan0.log.Info().Msgf("GL-fallback core: rejecting Vulkan SET_HW_RENDER so core retries with GL (%s)", Nan0.meta.LibPath)
		return false
	}
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
	if !Nan0.vulkan.enabled {
		Nan0.log.Warn().Msg("GET_HW_RENDER_INTERFACE called but Vulkan is not enabled")
		return false
	}
	// During the create_device phase, the real VulkanContext doesn't exist yet.
	// Return the bootstrap interface so Dolphin's swapchain wrapper has valid
	// function pointers (get_sync_index_mask, lock_queue, etc.).
	if Nan0.vulkan.ctx == nil {
		bootstrapPtr := vulkan.BootstrapInterfacePtr()
		if bootstrapPtr == nil {
			Nan0.log.Warn().Msg("GET_HW_RENDER_INTERFACE called but no bootstrap interface available")
			return false
		}
		*(*unsafe.Pointer)(data) = bootstrapPtr
		Nan0.log.Info().Msgf("GET_HW_RENDER_INTERFACE: returning BOOTSTRAP iface=%p (real ctx not ready yet)", bootstrapPtr)
		return true
	}
	iface := Nan0.vulkan.ctx.RenderInterface()
	if iface == nil {
		Nan0.log.Warn().Msg("GET_HW_RENDER_INTERFACE: RenderInterface() returned nil")
		return false
	}
	// data points to a retro_hw_render_interface* (void*) that the core
	// has passed by address: *(void**)data = &retro_hw_render_interface_vulkan
	*(*unsafe.Pointer)(data) = iface

	// DIAG: log the interface we're returning so we can verify the core got it
	ifaceC := (*C.struct_retro_hw_render_interface_vulkan)(iface)
	Nan0.log.Info().Msgf("GET_HW_RENDER_INTERFACE: returning iface=%p version=%d instance=%p gpu=%p device=%p queue=%p qi=%d handle=%p get_dev_pa=%p get_inst_pa=%p set_image=%p get_sync_idx=%p get_sync_mask=%p neg=%p",
		iface, ifaceC.interface_version, ifaceC.instance, ifaceC.gpu, ifaceC.device, ifaceC.queue,
		ifaceC.queue_index, ifaceC.handle,
		ifaceC.get_device_proc_addr, ifaceC.get_instance_proc_addr,
		ifaceC.set_image, ifaceC.get_sync_index, ifaceC.get_sync_index_mask,
		ifaceC.negotiation_interface)
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
			Nan0.log.Info().Msg("Vulkan fallback: creating frontend-owned Vulkan context")
			ctx, err = vulkan.NewVulkanContext(cfg)
			if err == nil {
				Nan0.log.Info().Msg("Vulkan fallback: frontend-owned Vulkan context created")
			} else {
				Nan0.log.Error().Err(err).Msg("Vulkan fallback: frontend-owned Vulkan context creation failed")
			}
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
		Nan0.log.Info().Msgf("Vulkan: about to call context_reset (cb=%p), ctx ready=%v, iface=%p",
			Nan0.Video.hw.context_reset, Nan0.vulkan.ctx != nil,
			func() unsafe.Pointer {
				if Nan0.vulkan.ctx != nil {
					return Nan0.vulkan.ctx.RenderInterface()
				}
				return nil
			}())
		// Install SIGSEGV crash handler to get exact backtrace if Dolphin crashes
		// during context_reset (known SIGSEGV at addr=0x2).
		// Remove it after so Dolphin's JIT fastmem SIGSEGV handler works normally.
		C.cloudplay_install_crash_handler()
		C.bridge_context_reset(Nan0.Video.hw.context_reset)
		C.cloudplay_remove_crash_handler()
		Nan0.log.Info().Msg("Vulkan: context_reset returned successfully")
	}
}

// initVulkanVideoWithNegotiation creates a VulkanContext by delegating device
// creation to the core via its negotiation interface callbacks.
func initVulkanVideoWithNegotiation(cfg vulkan.Config) (*vulkan.VulkanContext, error) {
	// Create an instance + pick a GPU, but let the core provide its own v2
	// instance when available. If LRPS2 requires instance-level extensions or
	// flags before device creation, delegating only the device step is too late.
	var (
		baseCtx *vulkan.InstanceOnlyContext
		err     error
	)
	if neg := (*C.struct_retro_hw_render_context_negotiation_interface_vulkan)(Nan0.vulkan.negotiationIface); neg != nil && uint32(neg.interface_version) >= 2 && neg.create_instance != nil {
		Nan0.log.Info().Msg("Vulkan negotiation: asking core to create instance via v2 callback")
		inst := C.call_create_instance(neg)
		if inst == nil {
			return nil, fmt.Errorf("negotiation: create_instance returned NULL")
		}
		baseCtx, err = vulkan.NewInstanceOnlyFromExisting(unsafe.Pointer(inst))
		if err != nil {
			return nil, fmt.Errorf("negotiation: failed to wrap core-created instance: %w", err)
		}
	} else {
		baseCtx, err = vulkan.NewInstanceOnly()
		if err != nil {
			return nil, fmt.Errorf("negotiation: failed to create instance: %w", err)
		}
	}

	// ── Create headless surface ────────────────────────────────────────────
	// Dolphin (and all Vulkan libretro cores) needs a valid VkSurfaceKHR to
	// create a swapchain.  Without a swapchain they never call
	// retro_video_refresh(), so zero frames reach our pipeline.
	//
	// We try VK_EXT_headless_surface first (no X11 needed), then fall back to
	// a 1×1 Xvfb window via VK_KHR_xlib_surface.
	surface, surfErr := vulkan.CreateHeadlessSurface(baseCtx.VkInstancePtr())
	if surfErr != nil {
		Nan0.log.Warn().Err(surfErr).Msg("Vulkan: failed to create headless surface, passing VK_NULL_HANDLE (core may not deliver frames)")
	} else {
		// Non-fatal: log whether the GPU advertises present support on this
		// surface.  Some headless drivers return false here even though
		// rendering still works.
		supportsPresent := vulkan.CheckSurfacePresent(baseCtx.VkPhysDevicePtr(), 0, surface)
		Nan0.log.Info().Msgf("Vulkan: headless surface created (GPU present-support on qf=0: %v)", supportsPresent)
	}

	// Build C-side surface handle to pass to call_create_device.
	// If surface creation failed, use VK_NULL_HANDLE (some cores handle it).
	var cSurface C.VkSurfaceKHR
	if surface != nil && surface.VkSurfacePtr() != nil {
		cSurface = (C.VkSurfaceKHR)(surface.VkSurfacePtr())
	}

	// ── Bootstrap render interface ─────────────────────────────────────────
	// Dolphin's CreateDevice → VulkanContext::Create → swapchain creation
	// needs a valid retro_hw_render_interface_vulkan pointer (it calls
	// vulkan->get_sync_index_mask, vulkan->lock_queue, etc. from the wrapped
	// Vulkan functions).  The real Provider doesn't exist yet, so we set up
	// a bootstrap interface with stub callbacks.  When Dolphin calls
	// GET_HW_RENDER_INTERFACE from inside CreateDevice, handleGetHWRenderInterface
	// returns this bootstrap interface.
	vulkan.InitBootstrapInterface(baseCtx.VkInstancePtr(), baseCtx.VkPhysDevicePtr())
	Nan0.log.Info().Msg("Vulkan: bootstrap interface initialized for create_device phase")

	// Call the core's create_device2 (or create_device) so it creates the
	// VkDevice with all the extensions it needs, and now with the surface so
	// it can create a swapchain and start calling retro_video_refresh().
	neg := (*C.struct_retro_hw_render_context_negotiation_interface_vulkan)(Nan0.vulkan.negotiationIface)
	Nan0.log.Info().Msgf("Vulkan negotiation: iface_version=%d create_device=%p create_device2=%p surface=%p", uint32(neg.interface_version), neg.create_device, neg.create_device2, cSurface)
	var vkCtx C.struct_retro_vulkan_context
	ok := C.call_create_device(
		neg,
		&vkCtx,
		(C.VkInstance)(baseCtx.VkInstancePtr()),
		(C.VkPhysicalDevice)(baseCtx.VkPhysDevicePtr()),
		cSurface,
	)
	if !bool(ok) {
		if surface != nil {
			surface.Destroy(baseCtx.VkInstancePtr())
		}
		baseCtx.DestroyInstanceOnly()
		return nil, fmt.Errorf("negotiation: create_device returned false")
	}

	extMemInjected := bool(C.extmem_was_injected() != 0)
	Nan0.log.Info().Msgf("Vulkan negotiation: device created by core, queue_family=%d, extmem_injected=%v",
		uint32(vkCtx.queue_family_index), extMemInjected)

	// Use the device/queue the core created.  Surface ownership is transferred
	// to the VulkanContext which will destroy it in Deinit().
	result := vulkan.NegotiationResult{
		Instance:            baseCtx.VkInstancePtr(),
		PhysDevice:          unsafe.Pointer(vkCtx.gpu),
		Device:              unsafe.Pointer(vkCtx.device),
		Queue:               unsafe.Pointer(vkCtx.queue),
		QueueFamily:         uint32(vkCtx.queue_family_index),
		Surface:             surface, // retained for cleanup
		ExternalMemoryReady: extMemInjected,
	}
	return vulkan.NewVulkanContextFromNegotiation(cfg, result)
}

// deinitVulkanVideo tears down the Vulkan context.  Safe to call even if the
// context was never initialised (e.g. core was never loaded).
func deinitVulkanVideo() {
	// Destroy the Provider (and unregister its cgo.Handle) BEFORE calling
	// context_destroy.  LRPS2's context_destroy callback may fire go_set_image
	// or other Go callbacks; with the handle already deleted, lookupProvider
	// returns nil and the callbacks are safe no-ops instead of segfaulting.
	if Nan0.vulkan.ctx != nil {
		Nan0.vulkan.ctx.DestroyProvider()
	}
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

// vulkanZeroCopyFd returns the Linux fd plus allocation size for the current
// frame's exportable Vulkan device memory after readVulkanFramebuffer has been
// called.
//
// Returns (-1, 0, err) when Phase 3 is not available.
// Used by the media pipeline to drive NVENC via CUDA without CPU copies.
func vulkanZeroCopyFd(w, h uint) (int, uint64, error) {
	if Nan0.vulkan.ctx == nil {
		return -1, 0, fmt.Errorf("vulkan: context not initialised")
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

// ZeroCopyFd returns the Linux fd plus allocation size for the exportable
// Vulkan device memory of the current frame. Delegates to vulkanZeroCopyFd.
func (n *Nanoarch) ZeroCopyFd(w, h uint) (int, uint64, error) {
	return vulkanZeroCopyFd(w, h)
}

// WaitZeroCopyBlit waits for the most recent async Vulkan zero-copy blit to
// complete before the encoder reads from the exported buffer.
func (n *Nanoarch) WaitZeroCopyBlit() error {
	if n.vulkan.ctx == nil {
		return nil
	}
	return n.vulkan.ctx.WaitZeroCopyBlit()
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

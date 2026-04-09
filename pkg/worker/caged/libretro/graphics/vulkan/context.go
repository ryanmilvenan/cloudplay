//go:build vulkan

// Package vulkan provides a headless Vulkan context for libretro HW rendering.
// It eliminates the X server dependency that the SDL/OpenGL path requires.
package vulkan

/*
#cgo LDFLAGS: -lvulkan
#include <vulkan/vulkan.h>
#include <stdlib.h>
#include <string.h>

static uint32_t cloudplay_vk_make_version(uint32_t major, uint32_t minor, uint32_t patch) {
    return VK_MAKE_VERSION(major, minor, patch);
}

// Submit a one-shot command buffer, wait for completion, then free.
// Done in C to avoid Go CGo pointer rules (VkSubmitInfo.pCommandBuffers = &cmd).
//
// We use vkQueueWaitIdle before AND after our submission to ensure:
// 1. The core's in-flight work is complete before we submit our readback
// 2. Our readback is complete before we return the pixel data
// This avoids the VK_ERROR_DEVICE_LOST that occurs when our submission
// races with the core's rendering submissions on the shared queue.
static VkResult cloudplay_submit_one_shot(
    VkDevice device, VkQueue queue, VkCommandPool pool, VkCommandBuffer cmd)
{
    vkEndCommandBuffer(cmd);

    // Wait for any in-flight work from the core to complete first.
    VkResult res = vkQueueWaitIdle(queue);
    if (res != VK_SUCCESS) {
        vkFreeCommandBuffers(device, pool, 1, &cmd);
        return res;
    }

    VkSubmitInfo submitInfo = {0};
    submitInfo.sType = VK_STRUCTURE_TYPE_SUBMIT_INFO;
    submitInfo.commandBufferCount = 1;
    submitInfo.pCommandBuffers = &cmd;

    res = vkQueueSubmit(queue, 1, &submitInfo, VK_NULL_HANDLE);
    if (res != VK_SUCCESS) {
        vkFreeCommandBuffers(device, pool, 1, &cmd);
        return res;
    }

    // Wait for our readback to complete.
    res = vkQueueWaitIdle(queue);
    vkFreeCommandBuffers(device, pool, 1, &cmd);
    return res;
}

// Submit a one-shot command buffer using a VkFence for synchronization.
//
// Unlike cloudplay_submit_one_shot, this version:
// - Does NOT call vkQueueWaitIdle before submission (caller must guarantee
//   the queue is idle, e.g. inside go_wait_sync_index)
// - Uses a VkFence to wait for just this submission to complete (narrower
//   than vkQueueWaitIdle which would stall any concurrent work)
//
// This is the safe path for readback on the negotiated-device path where
// Dolphin owns the queue but we know it's idle at wait_sync_index time.
static VkResult cloudplay_submit_one_shot_fenced(
    VkDevice device, VkQueue queue, VkCommandPool pool, VkCommandBuffer cmd)
{
    vkEndCommandBuffer(cmd);

    // Create a fence to wait on just our submission.
    VkFenceCreateInfo fenceInfo = {0};
    fenceInfo.sType = VK_STRUCTURE_TYPE_FENCE_CREATE_INFO;
    VkFence fence = VK_NULL_HANDLE;
    VkResult res = vkCreateFence(device, &fenceInfo, NULL, &fence);
    if (res != VK_SUCCESS) {
        vkFreeCommandBuffers(device, pool, 1, &cmd);
        return res;
    }

    VkSubmitInfo submitInfo = {0};
    submitInfo.sType = VK_STRUCTURE_TYPE_SUBMIT_INFO;
    submitInfo.commandBufferCount = 1;
    submitInfo.pCommandBuffers = &cmd;

    res = vkQueueSubmit(queue, 1, &submitInfo, fence);
    if (res != VK_SUCCESS) {
        vkDestroyFence(device, fence, NULL);
        vkFreeCommandBuffers(device, pool, 1, &cmd);
        return res;
    }

    // Wait for our readback to complete (5 second timeout).
    res = vkWaitForFences(device, 1, &fence, VK_TRUE, 5000000000ULL);
    vkDestroyFence(device, fence, NULL);
    vkFreeCommandBuffers(device, pool, 1, &cmd);
    return res;
}

// Submit a command buffer with a caller-provided fence (fire-and-forget).
// Does NOT call vkEndCommandBuffer (caller must do that).
// Does NOT wait for the fence.
// The caller is responsible for eventually waiting on the fence and
// freeing the command buffer.
static VkResult cloudplay_submit_with_fence(
    VkQueue queue, VkCommandBuffer cmd, VkFence fence)
{
    VkSubmitInfo submitInfo = {0};
    submitInfo.sType = VK_STRUCTURE_TYPE_SUBMIT_INFO;
    submitInfo.commandBufferCount = 1;
    submitInfo.pCommandBuffers = &cmd;
    return vkQueueSubmit(queue, 1, &submitInfo, fence);
}

// cloudplay_vk_create_instance_with_surface_exts creates a VkInstance with
// the surface extensions needed for headless surface creation:
//
//   VK_KHR_surface  (required by both surface extension paths)
//   VK_EXT_headless_surface  (pure headless, preferred)
//   VK_KHR_xlib_surface      (Xvfb fallback)
//
// We enumerate available instance extensions first and only enable those that
// are present, so the call degrades gracefully on any driver.
// Returns a bitmask of which surface extensions were actually enabled:
//   bit 0 = VK_KHR_surface
//   bit 1 = VK_EXT_headless_surface
//   bit 2 = VK_KHR_xlib_surface
static VkResult cloudplay_vk_create_instance(const char *app_name, const char *engine_name, VkInstance *out_instance) {
    VkApplicationInfo app_info = {
        .sType = VK_STRUCTURE_TYPE_APPLICATION_INFO,
        .pApplicationName = app_name,
        .applicationVersion = VK_MAKE_VERSION(1, 0, 0),
        .pEngineName = engine_name,
        .engineVersion = VK_MAKE_VERSION(1, 0, 0),
        .apiVersion = VK_API_VERSION_1_1,
    };

    // Desired surface extensions (in priority order).
    static const char *kWantedExts[] = {
        "VK_KHR_surface",
        "VK_EXT_headless_surface",
        "VK_KHR_xlib_surface",
    };
    static const int kNumWanted = 3;

    // Enumerate available instance extensions.
    uint32_t avail_count = 0;
    vkEnumerateInstanceExtensionProperties(NULL, &avail_count, NULL);
    VkExtensionProperties *avail = NULL;
    if (avail_count > 0) {
        avail = (VkExtensionProperties *)malloc(avail_count * sizeof(VkExtensionProperties));
        vkEnumerateInstanceExtensionProperties(NULL, &avail_count, avail);
    }

    // Filter wanted list down to those actually supported.
    const char *enabled[3];
    uint32_t enabled_count = 0;
    for (int wi = 0; wi < kNumWanted; wi++) {
        for (uint32_t ai = 0; ai < avail_count; ai++) {
            if (strcmp(avail[ai].extensionName, kWantedExts[wi]) == 0) {
                enabled[enabled_count++] = kWantedExts[wi];
                break;
            }
        }
    }
    if (avail) free(avail);

    VkInstanceCreateInfo instance_info = {
        .sType = VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO,
        .pApplicationInfo = &app_info,
        .enabledExtensionCount = enabled_count,
        .ppEnabledExtensionNames = enabled_count > 0 ? enabled : NULL,
    };

    return vkCreateInstance(&instance_info, NULL, out_instance);
}

static VkResult cloudplay_vk_create_device(
    VkPhysicalDevice phys_device,
    uint32_t queue_family_index,
    uint32_t extension_count,
    const char *const *extension_names,
    VkDevice *out_device)
{
    const float queue_priority = 1.0f;
    VkDeviceQueueCreateInfo queue_info = {
        .sType = VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,
        .queueFamilyIndex = queue_family_index,
        .queueCount = 1,
        .pQueuePriorities = &queue_priority,
    };

    VkDeviceCreateInfo device_info = {
        .sType = VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO,
        .queueCreateInfoCount = 1,
        .pQueueCreateInfos = &queue_info,
        .enabledExtensionCount = extension_count,
        .ppEnabledExtensionNames = extension_names,
    };

    return vkCreateDevice(phys_device, &device_info, NULL, out_device);
}

// Helper: find a queue family that supports graphics
static uint32_t find_graphics_queue_family(VkPhysicalDevice device) {
    uint32_t count = 0;
    vkGetPhysicalDeviceQueueFamilyProperties(device, &count, NULL);
    VkQueueFamilyProperties *props = (VkQueueFamilyProperties*)malloc(count * sizeof(VkQueueFamilyProperties));
    vkGetPhysicalDeviceQueueFamilyProperties(device, &count, props);
    uint32_t idx = UINT32_MAX;
    for (uint32_t i = 0; i < count; i++) {
        if (props[i].queueFlags & VK_QUEUE_GRAPHICS_BIT) {
            idx = i;
            break;
        }
    }
    free(props);
    return idx;
}

// Helper: pick physical device — prefer discrete GPU
static VkPhysicalDevice pick_physical_device(VkInstance instance) {
    uint32_t count = 0;
    if (vkEnumeratePhysicalDevices(instance, &count, NULL) != VK_SUCCESS || count == 0) {
        return VK_NULL_HANDLE;
    }
    VkPhysicalDevice *devices = (VkPhysicalDevice*)malloc(count * sizeof(VkPhysicalDevice));
    vkEnumeratePhysicalDevices(instance, &count, devices);

    VkPhysicalDevice chosen = devices[0]; // fallback: first available
    for (uint32_t i = 0; i < count; i++) {
        VkPhysicalDeviceProperties props;
        vkGetPhysicalDeviceProperties(devices[i], &props);
        if (props.deviceType == VK_PHYSICAL_DEVICE_TYPE_DISCRETE_GPU) {
            chosen = devices[i];
            break;
        }
    }
    free(devices);
    return chosen;
}
*/
import "C"
import (
	"errors"
	"fmt"
	"unsafe"
)

// Context is a headless Vulkan rendering context.
// It creates an instance, physical device, logical device and command pool
// without any window surface or swapchain — suitable for offscreen rendering.
type Context struct {
	Instance    C.VkInstance
	PhysDevice  C.VkPhysicalDevice
	Device      C.VkDevice
	Queue       C.VkQueue
	QueueFamily uint32
	CmdPool     C.VkCommandPool

	// Device memory properties cached for allocation helpers.
	MemProps C.VkPhysicalDeviceMemoryProperties

	// ExternalMemoryEnabled indicates that VK_KHR_external_memory_fd was
	// successfully requested at device creation time.  When true the Phase 3
	// zero-copy export path is available.
	ExternalMemoryEnabled bool

	// externalHandles is true when instance/device were created externally
	// (e.g. by the core via negotiation).  Destroy() will skip destroying them.
	externalHandles bool

	// getDeviceProcAddr is the PFN_vkGetDeviceProcAddr from the render
	// interface, which resolves to Dolphin's wrapped dispatch table.
	// When non-nil, submitOneShotViaDispatch uses it instead of the loader.
	getDeviceProcAddr C.PFN_vkGetDeviceProcAddr
}

// NewContext creates a headless Vulkan context.
// No surface extensions are requested — pure compute/render.
//
// Optional device extensions (e.g. VK_KHR_external_memory_fd for Phase 3) are
// requested via deviceExtensionsForExternalMemory().  On platforms where those
// extensions are unavailable the call succeeds with a baseline device, and
// Context.ExternalMemoryEnabled is false.
func NewContext() (*Context, error) {
	ctx := &Context{}

	// ── Instance ──────────────────────────────────────────────────────────
	appName := C.CString("cloudplay")
	engName := C.CString("libretro-vulkan")
	defer C.free(unsafe.Pointer(appName))
	defer C.free(unsafe.Pointer(engName))

	if res := C.cloudplay_vk_create_instance(appName, engName, &ctx.Instance); res != C.VK_SUCCESS {
		return nil, fmt.Errorf("vulkan: vkCreateInstance failed: %d", int(res))
	}

	// ── Physical Device ───────────────────────────────────────────────────
	ctx.PhysDevice = C.pick_physical_device(ctx.Instance)
	if ctx.PhysDevice == nil {
		C.vkDestroyInstance(ctx.Instance, nil)
		return nil, errors.New("vulkan: no physical device found")
	}

	// Cache memory properties for later allocations.
	C.vkGetPhysicalDeviceMemoryProperties(ctx.PhysDevice, &ctx.MemProps)

	// ── Queue family ──────────────────────────────────────────────────────
	qf := C.find_graphics_queue_family(ctx.PhysDevice)
	if qf == C.UINT32_MAX {
		C.vkDestroyInstance(ctx.Instance, nil)
		return nil, errors.New("vulkan: no graphics queue family found")
	}
	ctx.QueueFamily = uint32(qf)

	// Request optional Phase 3 extensions (VK_KHR_external_memory_fd on
	// linux+nvenc builds).  We first try with the extensions enabled; if the
	// driver rejects them we fall back to a baseline device.
	extNames := deviceExtensionsForExternalMemory()
	if len(extNames) > 0 {
		cExts := make([]*C.char, len(extNames))
		for i, e := range extNames {
			cExts[i] = C.CString(e)
		}

		extMem := C.malloc(C.size_t(len(cExts)) * C.size_t(unsafe.Sizeof(uintptr(0))))
		if extMem == nil {
			for _, p := range cExts {
				C.free(unsafe.Pointer(p))
			}
			C.vkDestroyInstance(ctx.Instance, nil)
			return nil, errors.New("vulkan: failed to allocate extension name array")
		}
		extArray := unsafe.Slice((**C.char)(extMem), len(cExts))
		for i, p := range cExts {
			extArray[i] = p
		}

		if res := C.cloudplay_vk_create_device(ctx.PhysDevice, qf, C.uint32_t(len(cExts)), (**C.char)(extMem), &ctx.Device); res == C.VK_SUCCESS {
			ctx.ExternalMemoryEnabled = true
		} else {
			// Extension(s) not supported on this driver — retry without them.
			if res2 := C.cloudplay_vk_create_device(ctx.PhysDevice, qf, 0, nil, &ctx.Device); res2 != C.VK_SUCCESS {
				C.free(extMem)
				for _, p := range cExts {
					C.free(unsafe.Pointer(p))
				}
				C.vkDestroyInstance(ctx.Instance, nil)
				return nil, fmt.Errorf("vulkan: vkCreateDevice failed: %d", int(res2))
			}
			// ExternalMemoryEnabled stays false.
		}
		C.free(extMem)
		for _, p := range cExts {
			C.free(unsafe.Pointer(p))
		}
	} else {
		if res := C.cloudplay_vk_create_device(ctx.PhysDevice, qf, 0, nil, &ctx.Device); res != C.VK_SUCCESS {
			C.vkDestroyInstance(ctx.Instance, nil)
			return nil, fmt.Errorf("vulkan: vkCreateDevice failed: %d", int(res))
		}
	}

	// Retrieve the graphics queue handle.
	C.vkGetDeviceQueue(ctx.Device, qf, 0, &ctx.Queue)

	// ── Command Pool ──────────────────────────────────────────────────────
	poolInfo := C.VkCommandPoolCreateInfo{
		sType:            C.VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,
		queueFamilyIndex: qf,
		flags:            C.VK_COMMAND_POOL_CREATE_RESET_COMMAND_BUFFER_BIT,
	}

	if res := C.vkCreateCommandPool(ctx.Device, &poolInfo, nil, &ctx.CmdPool); res != C.VK_SUCCESS {
		C.vkDestroyDevice(ctx.Device, nil)
		C.vkDestroyInstance(ctx.Instance, nil)
		return nil, fmt.Errorf("vulkan: vkCreateCommandPool failed: %d", int(res))
	}

	return ctx, nil
}

// InstanceOnlyContext holds a VkInstance + VkPhysicalDevice for use before
// the device is created by the core via negotiation.
type InstanceOnlyContext struct {
	instance   C.VkInstance
	physDevice C.VkPhysicalDevice
}

// NewInstanceOnly creates a VkInstance and picks a physical device, but does
// NOT create a VkDevice.  Use when the core will create the device itself via
// the negotiation interface.
func NewInstanceOnly() (*InstanceOnlyContext, error) {
	appName := C.CString("cloudplay")
	engName := C.CString("libretro-vulkan")
	defer C.free(unsafe.Pointer(appName))
	defer C.free(unsafe.Pointer(engName))

	var instance C.VkInstance
	if res := C.cloudplay_vk_create_instance(appName, engName, &instance); res != C.VK_SUCCESS {
		return nil, fmt.Errorf("vulkan: vkCreateInstance failed: %d", int(res))
	}
	return NewInstanceOnlyFromExisting(unsafe.Pointer(instance))
}

// NewInstanceOnlyFromExisting wraps an already-created VkInstance and picks a
// physical device from it. Used when the core creates the instance via v2
// negotiation and the frontend still needs to create a surface and choose a
// GPU before delegating device creation.
func NewInstanceOnlyFromExisting(instance unsafe.Pointer) (*InstanceOnlyContext, error) {
	ctx := &InstanceOnlyContext{instance: (C.VkInstance)(instance)}
	ctx.physDevice = C.pick_physical_device(ctx.instance)
	if ctx.physDevice == nil {
		if ctx.instance != nil {
			C.vkDestroyInstance(ctx.instance, nil)
			ctx.instance = nil
		}
		return nil, errors.New("vulkan: no physical device found")
	}
	return ctx, nil
}

// VkInstancePtr returns the VkInstance handle as unsafe.Pointer for cross-package use.
func (c *InstanceOnlyContext) VkInstancePtr() unsafe.Pointer { return unsafe.Pointer(c.instance) }

// VkPhysDevicePtr returns the VkPhysicalDevice handle as unsafe.Pointer for cross-package use.
func (c *InstanceOnlyContext) VkPhysDevicePtr() unsafe.Pointer { return unsafe.Pointer(c.physDevice) }

// DestroyInstanceOnly frees the VkInstance.
// Should be called only if device creation fails (i.e. the instance won't be
// handed to a Context via NewContextFromHandles).
func (c *InstanceOnlyContext) DestroyInstanceOnly() {
	if c.instance != nil {
		C.vkDestroyInstance(c.instance, nil)
		c.instance = nil
	}
}

// NewContextFromHandles creates a Context that wraps externally-created
// Vulkan handles (e.g. created by the core via the negotiation interface).
//
// The caller is responsible for ensuring the handles are valid and that they
// outlive the returned Context.  The caller must NOT destroy these handles
// through normal Context.Destroy() — only the CmdPool will be destroyed.
//
// Use this when the core (e.g. Dolphin) creates its own VkDevice via the
// RETRO_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_VULKAN callbacks.
func NewContextFromHandles(
	instance C.VkInstance,
	physDevice C.VkPhysicalDevice,
	device C.VkDevice,
	queue C.VkQueue,
	queueFamily uint32,
) (*Context, error) {
	ctx := &Context{
		Instance:    instance,
		PhysDevice:  physDevice,
		Device:      device,
		Queue:       queue,
		QueueFamily: queueFamily,
	}
	// Cache memory properties.
	C.vkGetPhysicalDeviceMemoryProperties(physDevice, &ctx.MemProps)

	// Create command pool.
	poolInfo := C.VkCommandPoolCreateInfo{
		sType:            C.VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,
		queueFamilyIndex: C.uint32_t(queueFamily),
		flags:            C.VK_COMMAND_POOL_CREATE_RESET_COMMAND_BUFFER_BIT,
	}
	if res := C.vkCreateCommandPool(ctx.Device, &poolInfo, nil, &ctx.CmdPool); res != C.VK_SUCCESS {
		return nil, fmt.Errorf("vulkan: NewContextFromHandles: vkCreateCommandPool failed: %d", int(res))
	}
	// Mark as external so Destroy() doesn't destroy the device/instance.
	ctx.externalHandles = true
	return ctx, nil
}

// Destroy releases all Vulkan resources owned by this context.
// Must be called after all dependent resources (images, buffers) are destroyed.
// When externalHandles is true (context created via NewContextFromHandles),
// only the command pool is destroyed; the device and instance are left intact
// since they are owned by the caller (core).
func (c *Context) Destroy() {
	if c.CmdPool != nil {
		C.vkDestroyCommandPool(c.Device, c.CmdPool, nil)
		c.CmdPool = nil
	}
	if c.externalHandles {
		// Device and instance are owned by the core; don't destroy them.
		c.Device = nil
		c.Instance = nil
		return
	}
	if c.Device != nil {
		C.vkDestroyDevice(c.Device, nil)
		c.Device = nil
	}
	if c.Instance != nil {
		C.vkDestroyInstance(c.Instance, nil)
		c.Instance = nil
	}
}

// findMemoryType returns the index of a memory type that satisfies both
// the type filter bitmask and the required property flags.
func (c *Context) findMemoryType(typeFilter uint32, props C.VkMemoryPropertyFlags) (uint32, error) {
	for i := uint32(0); i < uint32(c.MemProps.memoryTypeCount); i++ {
		if (typeFilter>>i)&1 == 1 {
			if c.MemProps.memoryTypes[i].propertyFlags&props == props {
				return i, nil
			}
		}
	}
	return 0, errors.New("vulkan: no suitable memory type found")
}

// allocateMemory allocates device memory suitable for the given requirements.
func (c *Context) allocateMemory(reqs C.VkMemoryRequirements, props C.VkMemoryPropertyFlags) (C.VkDeviceMemory, error) {
	memTypeIdx, err := c.findMemoryType(uint32(reqs.memoryTypeBits), props)
	if err != nil {
		return nil, err
	}
	allocInfo := C.VkMemoryAllocateInfo{
		sType:           C.VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
		allocationSize:  reqs.size,
		memoryTypeIndex: C.uint32_t(memTypeIdx),
	}
	var mem C.VkDeviceMemory
	if res := C.vkAllocateMemory(c.Device, &allocInfo, nil, &mem); res != C.VK_SUCCESS {
		return nil, fmt.Errorf("vulkan: vkAllocateMemory failed: %d", int(res))
	}
	return mem, nil
}

// beginOneShot allocates and begins a one-shot command buffer.
func (c *Context) beginOneShot() (C.VkCommandBuffer, error) {
	allocInfo := C.VkCommandBufferAllocateInfo{
		sType:              C.VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,
		commandPool:        c.CmdPool,
		level:              C.VK_COMMAND_BUFFER_LEVEL_PRIMARY,
		commandBufferCount: 1,
	}
	var cmd C.VkCommandBuffer
	if res := C.vkAllocateCommandBuffers(c.Device, &allocInfo, &cmd); res != C.VK_SUCCESS {
		return nil, fmt.Errorf("vulkan: vkAllocateCommandBuffers: %d", int(res))
	}
	beginInfo := C.VkCommandBufferBeginInfo{
		sType: C.VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
		flags: C.VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT,
	}
	C.vkBeginCommandBuffer(cmd, &beginInfo)
	return cmd, nil
}

// submitOneShot submits a one-shot command buffer and waits for it with a
// fence (narrower than vkQueueWaitIdle which stalls all in-flight work).
//
// NOTE: The actual submit + fence logic is in the C helper
// cloudplay_submit_one_shot to avoid Go 1.21+ CGo pointer rules violations
// (VkSubmitInfo.pCommandBuffers = &cmd would be a Go pointer in a struct
// passed to C).
func (c *Context) submitOneShot(cmd C.VkCommandBuffer) error {
	// On the negotiated-device path, use the fenced submit which doesn't
	// call vkQueueWaitIdle (the caller guarantees the queue is idle).
	if c.externalHandles {
		return c.submitOneShotFenced(cmd)
	}
	res := C.cloudplay_submit_one_shot(c.Device, c.Queue, c.CmdPool, cmd)
	if res != C.VK_SUCCESS {
		return fmt.Errorf("vulkan: submitOneShot: %d", int(res))
	}
	return nil
}

// submitOneShotFenced uses a VkFence instead of vkQueueWaitIdle for
// synchronization.  Avoids interfering with Dolphin's fence tracking.
func (c *Context) submitOneShotFenced(cmd C.VkCommandBuffer) error {
	res := C.cloudplay_submit_one_shot_fenced(c.Device, c.Queue, c.CmdPool, cmd)
	if res != C.VK_SUCCESS {
		return fmt.Errorf("vulkan: submitOneShotFenced: %d", int(res))
	}
	return nil
}

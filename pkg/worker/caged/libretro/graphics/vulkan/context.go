//go:build vulkan

// Package vulkan provides a headless Vulkan context for libretro HW rendering.
// It eliminates the X server dependency that the SDL/OpenGL path requires.
package vulkan

/*
#cgo LDFLAGS: -lvulkan
#include <vulkan/vulkan.h>
#include <stdlib.h>
#include <string.h>

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
}

// NewContext creates a headless Vulkan context.
// No surface extensions are requested — pure compute/render.
func NewContext() (*Context, error) {
	ctx := &Context{}

	// ── Instance ──────────────────────────────────────────────────────────
	appName := C.CString("cloudplay")
	engName := C.CString("libretro-vulkan")
	defer C.free(unsafe.Pointer(appName))
	defer C.free(unsafe.Pointer(engName))

	appInfo := C.VkApplicationInfo{
		sType:              C.VK_STRUCTURE_TYPE_APPLICATION_INFO,
		pApplicationName:   appName,
		applicationVersion: C.VK_MAKE_VERSION(1, 0, 0),
		pEngineName:        engName,
		engineVersion:      C.VK_MAKE_VERSION(1, 0, 0),
		apiVersion:         C.VK_API_VERSION_1_1,
	}

	instanceInfo := C.VkInstanceCreateInfo{
		sType:            C.VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO,
		pApplicationInfo: &appInfo,
	}

	if res := C.vkCreateInstance(&instanceInfo, nil, &ctx.Instance); res != C.VK_SUCCESS {
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

	// ── Logical Device ────────────────────────────────────────────────────
	queuePriority := C.float(1.0)
	queueInfo := C.VkDeviceQueueCreateInfo{
		sType:            C.VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,
		queueFamilyIndex: qf,
		queueCount:       1,
		pQueuePriorities: &queuePriority,
	}

	deviceInfo := C.VkDeviceCreateInfo{
		sType:                C.VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO,
		queueCreateInfoCount: 1,
		pQueueCreateInfos:    &queueInfo,
	}

	if res := C.vkCreateDevice(ctx.PhysDevice, &deviceInfo, nil, &ctx.Device); res != C.VK_SUCCESS {
		C.vkDestroyInstance(ctx.Instance, nil)
		return nil, fmt.Errorf("vulkan: vkCreateDevice failed: %d", int(res))
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

// Destroy releases all Vulkan resources owned by this context.
// Must be called after all dependent resources (images, buffers) are destroyed.
func (c *Context) Destroy() {
	if c.CmdPool != nil {
		C.vkDestroyCommandPool(c.Device, c.CmdPool, nil)
		c.CmdPool = nil
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

// submitOneShot submits and waits for a one-shot command buffer, then frees it.
func (c *Context) submitOneShot(cmd C.VkCommandBuffer) error {
	C.vkEndCommandBuffer(cmd)
	submitInfo := C.VkSubmitInfo{
		sType:                C.VK_STRUCTURE_TYPE_SUBMIT_INFO,
		commandBufferCount:   1,
		pCommandBuffers:      &cmd,
	}
	if res := C.vkQueueSubmit(c.Queue, 1, &submitInfo, nil); res != C.VK_SUCCESS {
		C.vkFreeCommandBuffers(c.Device, c.CmdPool, 1, &cmd)
		return fmt.Errorf("vulkan: vkQueueSubmit: %d", int(res))
	}
	C.vkQueueWaitIdle(c.Queue)
	C.vkFreeCommandBuffers(c.Device, c.CmdPool, 1, &cmd)
	return nil
}

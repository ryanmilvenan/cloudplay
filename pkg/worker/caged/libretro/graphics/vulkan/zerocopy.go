//go:build vulkan && linux && nvenc

package vulkan

// Phase 3 zero-copy path — Vulkan external memory → fd → CUDA → NVENC.
//
// Architecture
// ────────────
//  1. At startup, ZeroCopyBuffer.Init() allocates a VkBuffer backed by
//     VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT memory whose allocation chain
//     includes VkExportMemoryAllocateInfo.  This gives NVIDIA's driver an
//     exportable OPAQUE_FD handle.
//
//  2. On every frame:
//     a. vkCmdCopyImageToBuffer copies the core's rendered VkImage into this
//        exportable device-local buffer (GPU-to-GPU; no CPU involvement).
//     b. We export the memory fd via vkGetMemoryFdKHR (done once; the fd
//        stays valid for the lifetime of the allocation).
//     c. CUDA imports the fd via cuMemImportFromShareableHandle → gives us a
//        CUdeviceptr in the same physical memory.
//     d. FFmpeg's nvenc encoder receives that CUdeviceptr directly via an
//        AVFrame with a custom data pointer set to a CUDA device address,
//        bypassing all CPU-side data movement.
//
// Current state (Phase 3c)
// ─────────────────────────
//  • Steps 1 and 2a (VkBuffer allocation + vkCmdCopyImageToBuffer) are fully
//    implemented here.
//  • The fd export (ExportMemoryFd) calls vkGetMemoryFdKHR and returns the
//    Linux fd so the CUDA layer can import it.
//  • Steps 2c-2d (CUDA import + EncodeFromDevPtr) live in
//    pkg/encoder/nvenc/nvenc_cuda.go; the CUexternalMemory lifetime is now
//    tracked via nvenc.ExtMemHandle and released on session teardown.
//  • GPU RGBA→NV12 colour conversion (Phase 3c): two embedded PTX kernels
//    (BT.601 studio-swing) are JIT-compiled at first use; no nvcc needed.

/*
#cgo LDFLAGS: -lvulkan
#include <vulkan/vulkan.h>
#include <vulkan/vulkan_core.h>

// VK_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD_BIT is defined in Vulkan 1.1+.
// Use the raw integer to stay header-portable.
#ifndef VK_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD_BIT
#define VK_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD_BIT 0x00000001
#endif

#include <stdlib.h>
#include <string.h>

// export_memory_fd calls vkGetMemoryFdKHR via the device's proc-addr.
// Returns -1 on failure.
static int export_memory_fd(VkDevice device, VkDeviceMemory memory) {
    PFN_vkGetMemoryFdKHR fn =
        (PFN_vkGetMemoryFdKHR)vkGetDeviceProcAddr(device, "vkGetMemoryFdKHR");
    if (!fn) return -1;

    VkMemoryGetFdInfoKHR info = {0};
    info.sType      = VK_STRUCTURE_TYPE_MEMORY_GET_FD_INFO_KHR;
    info.memory     = memory;
    info.handleType = VK_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD_BIT;

    int fd = -1;
    if (fn(device, &info, &fd) != VK_SUCCESS) return -1;
    return fd;
}

// blit_image_to_buffer records a vkCmdCopyImageToBuffer from srcImage
// (in currentLayout) into dstBuffer.  Performs the necessary
// TRANSFER_SRC_OPTIMAL transition inline.
static void blit_image_to_buffer(
    VkCommandBuffer cmd,
    VkImage         srcImage,
    VkImageLayout   currentLayout,
    VkBuffer        dstBuffer,
    uint32_t        width,
    uint32_t        height)
{
    // Transition to TRANSFER_SRC if needed.
    if (currentLayout != VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL) {
        VkImageMemoryBarrier barrier = {0};
        barrier.sType               = VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER;
        barrier.oldLayout           = currentLayout;
        barrier.newLayout           = VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL;
        barrier.srcQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED;
        barrier.dstQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED;
        barrier.image               = srcImage;
        barrier.subresourceRange.aspectMask     = VK_IMAGE_ASPECT_COLOR_BIT;
        barrier.subresourceRange.levelCount     = 1;
        barrier.subresourceRange.layerCount     = 1;
        barrier.srcAccessMask = VK_ACCESS_SHADER_WRITE_BIT | VK_ACCESS_COLOR_ATTACHMENT_WRITE_BIT;
        barrier.dstAccessMask = VK_ACCESS_TRANSFER_READ_BIT;
        vkCmdPipelineBarrier(cmd,
            VK_PIPELINE_STAGE_COLOR_ATTACHMENT_OUTPUT_BIT | VK_PIPELINE_STAGE_FRAGMENT_SHADER_BIT,
            VK_PIPELINE_STAGE_TRANSFER_BIT,
            0, 0, NULL, 0, NULL, 1, &barrier);
    }

    VkBufferImageCopy region = {0};
    region.imageSubresource.aspectMask = VK_IMAGE_ASPECT_COLOR_BIT;
    region.imageSubresource.layerCount = 1;
    region.imageExtent.width  = width;
    region.imageExtent.height = height;
    region.imageExtent.depth  = 1;
    vkCmdCopyImageToBuffer(cmd, srcImage,
        VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL, dstBuffer, 1, &region);

    // Transition back so the core can reuse the image.
    VkImageMemoryBarrier back = {0};
    back.sType               = VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER;
    back.oldLayout           = VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL;
    back.newLayout           = VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL;
    back.srcQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED;
    back.dstQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED;
    back.image               = srcImage;
    back.subresourceRange.aspectMask = VK_IMAGE_ASPECT_COLOR_BIT;
    back.subresourceRange.levelCount = 1;
    back.subresourceRange.layerCount = 1;
    back.srcAccessMask = VK_ACCESS_TRANSFER_READ_BIT;
    back.dstAccessMask = VK_ACCESS_SHADER_READ_BIT;
    vkCmdPipelineBarrier(cmd,
        VK_PIPELINE_STAGE_TRANSFER_BIT,
        VK_PIPELINE_STAGE_FRAGMENT_SHADER_BIT,
        0, 0, NULL, 0, NULL, 1, &back);
}
*/
import "C"
import (
	"fmt"
	"unsafe"
)

// ZeroCopyBuffer is a device-local Vulkan buffer backed by exportable memory.
// It is used as the blit destination in Phase 3: the core's rendered VkImage
// is blitted here (GPU-to-GPU), and the underlying memory is exported as a
// Linux fd so CUDA can import it without any CPU involvement.
type ZeroCopyBuffer struct {
	ctx    *Context
	buf    C.VkBuffer
	mem    C.VkDeviceMemory
	size   C.VkDeviceSize
	width  uint32
	height uint32

	// memFd is the exported OS file descriptor for the device memory.
	// -1 until ExportMemoryFd is called successfully.
	memFd int
}

// NewZeroCopyBuffer allocates a device-local exportable buffer for w×h RGBA8.
// Returns an error if the device was created without external-memory support.
func NewZeroCopyBuffer(ctx *Context, w, h uint32) (*ZeroCopyBuffer, error) {
	if !ctx.ExternalMemoryEnabled {
		return nil, fmt.Errorf("zerocopy: VK_KHR_external_memory_fd not available on this device")
	}

	size := C.VkDeviceSize(w * h * 4) // 4 bytes per RGBA8 pixel
	zc := &ZeroCopyBuffer{
		ctx:    ctx,
		size:   size,
		width:  w,
		height: h,
		memFd:  -1,
	}

	// Create the buffer.
	bufInfo := C.VkBufferCreateInfo{
		sType: C.VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
		size:  size,
		usage: C.VK_BUFFER_USAGE_TRANSFER_DST_BIT,
	}
	if res := C.vkCreateBuffer(ctx.Device, &bufInfo, nil, &zc.buf); res != C.VK_SUCCESS {
		return nil, fmt.Errorf("zerocopy: vkCreateBuffer: %d", int(res))
	}

	var reqs C.VkMemoryRequirements
	C.vkGetBufferMemoryRequirements(ctx.Device, zc.buf, &reqs)

	// Allocate device-local exportable memory.
	// Chain: VkMemoryAllocateInfo → VkExportMemoryAllocateInfo
	exportInfo := C.VkExportMemoryAllocateInfo{
		sType:       C.VK_STRUCTURE_TYPE_EXPORT_MEMORY_ALLOCATE_INFO,
		handleTypes: C.VK_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD_BIT,
	}
	memTypeIdx, err := ctx.findMemoryType(
		uint32(reqs.memoryTypeBits),
		C.VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT,
	)
	if err != nil {
		C.vkDestroyBuffer(ctx.Device, zc.buf, nil)
		return nil, fmt.Errorf("zerocopy: findMemoryType: %w", err)
	}
	allocInfo := C.VkMemoryAllocateInfo{
		sType:           C.VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
		allocationSize:  reqs.size,
		memoryTypeIndex: C.uint32_t(memTypeIdx),
		pNext:           unsafe.Pointer(&exportInfo),
	}
	if res := C.vkAllocateMemory(ctx.Device, &allocInfo, nil, &zc.mem); res != C.VK_SUCCESS {
		C.vkDestroyBuffer(ctx.Device, zc.buf, nil)
		return nil, fmt.Errorf("zerocopy: vkAllocateMemory: %d", int(res))
	}

	if res := C.vkBindBufferMemory(ctx.Device, zc.buf, zc.mem, 0); res != C.VK_SUCCESS {
		C.vkFreeMemory(ctx.Device, zc.mem, nil)
		C.vkDestroyBuffer(ctx.Device, zc.buf, nil)
		return nil, fmt.Errorf("zerocopy: vkBindBufferMemory: %d", int(res))
	}

	return zc, nil
}

// BlitFrom records a GPU-to-GPU copy from srcImage into this buffer.
// The command is submitted immediately and the device is synchronised before
// returning (conservative; can be made async with a fence in a future pass).
//
// After this call the buffer contains RGBA8 pixel data ready for CUDA import.
func (zc *ZeroCopyBuffer) BlitFrom(srcImage C.VkImage, layout C.VkImageLayout) error {
	cmd, err := zc.ctx.beginOneShot()
	if err != nil {
		return err
	}
	C.blit_image_to_buffer(cmd, srcImage, layout, zc.buf, C.uint32_t(zc.width), C.uint32_t(zc.height))
	return zc.ctx.submitOneShot(cmd)
}

// ExportMemoryFd returns a Linux file descriptor for the buffer's device
// memory.  The fd is created once and cached; CUDA can import it via
// cuMemImportFromShareableHandle.
//
// The caller owns the fd on first call; subsequent calls return the same
// cached value (CUDA keeps the mapping alive via the import handle).
//
// Phase 3c: the fd is owned by this ZeroCopyBuffer.  CUDA imports it via
// cuImportExternalMemory (which dup()s the fd internally on Linux), so this
// fd can be closed after import.  The ExtMemHandle returned by
// nvenc.ImportExternalMemory must be released via nvenc.ReleaseExternalMemory
// when the session ends (handled by lazyZeroCopyNVENC.Destroy).
func (zc *ZeroCopyBuffer) ExportMemoryFd() (int, error) {
	if zc.memFd >= 0 {
		return zc.memFd, nil
	}
	fd := int(C.export_memory_fd(zc.ctx.Device, zc.mem))
	if fd < 0 {
		return -1, fmt.Errorf("zerocopy: vkGetMemoryFdKHR failed")
	}
	zc.memFd = fd
	return fd, nil
}

// Size returns the buffer allocation size in bytes.
func (zc *ZeroCopyBuffer) Size() uint64 { return uint64(zc.size) }

// Destroy frees the Vulkan buffer and memory.
// The exported fd (if any) must be closed separately by the CUDA layer.
func (zc *ZeroCopyBuffer) Destroy() {
	if zc.buf != nil {
		C.vkDestroyBuffer(zc.ctx.Device, zc.buf, nil)
		zc.buf = nil
	}
	if zc.mem != nil {
		C.vkFreeMemory(zc.ctx.Device, zc.mem, nil)
		zc.mem = nil
	}
}

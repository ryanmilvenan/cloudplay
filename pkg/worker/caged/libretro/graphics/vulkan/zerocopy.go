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

static VkResult cloudplay_submit_one_shot_wait_sems(
    VkDevice device,
    VkQueue queue,
    VkCommandPool pool,
    VkCommandBuffer cmd,
    uint32_t waitSemaphoreCount,
    const VkSemaphore *waitSemaphores)
{
    vkEndCommandBuffer(cmd);

    VkResult res = vkQueueWaitIdle(queue);
    if (res != VK_SUCCESS) {
        vkFreeCommandBuffers(device, pool, 1, &cmd);
        return res;
    }

    VkPipelineStageFlags *waitStages = NULL;
    if (waitSemaphoreCount > 0) {
        waitStages = (VkPipelineStageFlags *)calloc(waitSemaphoreCount, sizeof(VkPipelineStageFlags));
        if (!waitStages) {
            vkFreeCommandBuffers(device, pool, 1, &cmd);
            return VK_ERROR_OUT_OF_HOST_MEMORY;
        }
        for (uint32_t i = 0; i < waitSemaphoreCount; ++i) {
            waitStages[i] = VK_PIPELINE_STAGE_TRANSFER_BIT;
        }
    }

    VkSubmitInfo submitInfo = {0};
    submitInfo.sType = VK_STRUCTURE_TYPE_SUBMIT_INFO;
    submitInfo.waitSemaphoreCount = waitSemaphoreCount;
    submitInfo.pWaitSemaphores = waitSemaphores;
    submitInfo.pWaitDstStageMask = waitStages;
    submitInfo.commandBufferCount = 1;
    submitInfo.pCommandBuffers = &cmd;

    res = vkQueueSubmit(queue, 1, &submitInfo, VK_NULL_HANDLE);
    if (waitStages) free(waitStages);
    if (res != VK_SUCCESS) {
        vkFreeCommandBuffers(device, pool, 1, &cmd);
        return res;
    }

    res = vkQueueWaitIdle(queue);
    vkFreeCommandBuffers(device, pool, 1, &cmd);
    return res;
}

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
    uint32_t        height,
    uint32_t        srcQueueFamily,
    uint32_t        dstQueueFamily)
{
    uint32_t acquireSrcQF = srcQueueFamily;
    uint32_t acquireDstQF = dstQueueFamily;
    if (srcQueueFamily == VK_QUEUE_FAMILY_IGNORED ||
        dstQueueFamily == VK_QUEUE_FAMILY_IGNORED ||
        srcQueueFamily == dstQueueFamily) {
        acquireSrcQF = VK_QUEUE_FAMILY_IGNORED;
        acquireDstQF = VK_QUEUE_FAMILY_IGNORED;
    }
    // Transition to TRANSFER_SRC if needed.
    // For negotiated-device LRPS2 we do not fully control which pipeline stage
    // last produced the image. A narrow fragment/color barrier can leave the
    // copy seeing stale or zeroed contents even when the layout value itself is
    // correct, so make the source-visibility side conservative here.
    if (currentLayout != VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL) {
        VkImageMemoryBarrier barrier = {0};
        barrier.sType               = VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER;
        barrier.oldLayout           = currentLayout;
        barrier.newLayout           = VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL;
        barrier.srcQueueFamilyIndex = acquireSrcQF;
        barrier.dstQueueFamilyIndex = acquireDstQF;
        barrier.image               = srcImage;
        barrier.subresourceRange.aspectMask     = VK_IMAGE_ASPECT_COLOR_BIT;
        barrier.subresourceRange.levelCount     = 1;
        barrier.subresourceRange.layerCount     = 1;
        barrier.srcAccessMask = VK_ACCESS_MEMORY_WRITE_BIT | VK_ACCESS_MEMORY_READ_BIT;
        barrier.dstAccessMask = VK_ACCESS_TRANSFER_READ_BIT;
        vkCmdPipelineBarrier(cmd,
            VK_PIPELINE_STAGE_ALL_COMMANDS_BIT,
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

    // Make the copied buffer contents visible outside the transfer stage.
    // This matters more for the exportable device-local zero-copy path than
    // for CPU staging readback: CUDA imports and reads this VkBuffer's memory
    // after the submit completes, so we need an explicit write->read barrier
    // on the destination buffer itself.
    VkBufferMemoryBarrier bufBarrier = {0};
    bufBarrier.sType               = VK_STRUCTURE_TYPE_BUFFER_MEMORY_BARRIER;
    bufBarrier.srcQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED;
    bufBarrier.dstQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED;
    bufBarrier.buffer              = dstBuffer;
    bufBarrier.offset              = 0;
    bufBarrier.size                = VK_WHOLE_SIZE;
    bufBarrier.srcAccessMask       = VK_ACCESS_TRANSFER_WRITE_BIT;
    bufBarrier.dstAccessMask       = VK_ACCESS_MEMORY_READ_BIT;
    vkCmdPipelineBarrier(cmd,
        VK_PIPELINE_STAGE_TRANSFER_BIT,
        VK_PIPELINE_STAGE_ALL_COMMANDS_BIT,
        0, 0, NULL, 1, &bufBarrier, 0, NULL);

    // Transition back so the core can reuse the image.
    VkImageMemoryBarrier back = {0};
    back.sType               = VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER;
    back.oldLayout           = VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL;
    back.newLayout           = VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL;
    back.srcQueueFamilyIndex = acquireDstQF;
    back.dstQueueFamilyIndex = acquireSrcQF;
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

	// Async blit support: persistent fence + command buffer.
	blitFence   C.VkFence         // reused each frame; nil until first blit
	blitCmd     C.VkCommandBuffer // pre-allocated; nil until first blit
	blitPending bool              // true after BlitFromAsync, false after WaitBlit
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
	}

	// Create the buffer.
	// External-memory Vulkan paths require the handle type to be declared on
	// the buffer/image object itself, not just on the memory allocation. Without
	// VkExternalMemoryBufferCreateInfo here, vkGetMemoryFdKHR may succeed while
	// the exported memory path still behaves incorrectly.
	cExtBufInfo := (*C.VkExternalMemoryBufferCreateInfo)(C.calloc(1, C.size_t(C.sizeof_VkExternalMemoryBufferCreateInfo)))
	cExtBufInfo.sType = C.VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_BUFFER_CREATE_INFO
	cExtBufInfo.handleTypes = C.VK_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD_BIT
	bufInfo := C.VkBufferCreateInfo{
		sType: C.VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
		pNext: unsafe.Pointer(cExtBufInfo),
		size:  size,
		usage: C.VK_BUFFER_USAGE_TRANSFER_DST_BIT,
	}
	if res := C.vkCreateBuffer(ctx.Device, &bufInfo, nil, &zc.buf); res != C.VK_SUCCESS {
		C.free(unsafe.Pointer(cExtBufInfo))
		return nil, fmt.Errorf("zerocopy: vkCreateBuffer: %d", int(res))
	}
	C.free(unsafe.Pointer(cExtBufInfo))

	var reqs C.VkMemoryRequirements
	C.vkGetBufferMemoryRequirements(ctx.Device, zc.buf, &reqs)
	zc.size = reqs.size

	// Allocate device-local exportable memory.
	// Chain: VkMemoryAllocateInfo → VkMemoryDedicatedAllocateInfo → VkExportMemoryAllocateInfo
	//
	// NOTE: These structs are allocated via C.calloc to avoid the Go 1.21+ CGo
	// rule that forbids passing Go pointers containing other Go pointers to C.
	cExportInfo := (*C.VkExportMemoryAllocateInfo)(C.calloc(1, C.size_t(C.sizeof_VkExportMemoryAllocateInfo)))
	cExportInfo.sType = C.VK_STRUCTURE_TYPE_EXPORT_MEMORY_ALLOCATE_INFO
	cExportInfo.handleTypes = C.VK_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD_BIT

	cDedicatedInfo := (*C.VkMemoryDedicatedAllocateInfo)(C.calloc(1, C.size_t(C.sizeof_VkMemoryDedicatedAllocateInfo)))
	cDedicatedInfo.sType = C.VK_STRUCTURE_TYPE_MEMORY_DEDICATED_ALLOCATE_INFO
	cDedicatedInfo.buffer = zc.buf
	cDedicatedInfo.image = nil
	cDedicatedInfo.pNext = unsafe.Pointer(cExportInfo)

	memTypeIdx, err := ctx.findMemoryType(
		uint32(reqs.memoryTypeBits),
		C.VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT,
	)
	if err != nil {
		C.free(unsafe.Pointer(cDedicatedInfo))
		C.free(unsafe.Pointer(cExportInfo))
		C.vkDestroyBuffer(ctx.Device, zc.buf, nil)
		return nil, fmt.Errorf("zerocopy: findMemoryType: %w", err)
	}
	cAllocInfo := (*C.VkMemoryAllocateInfo)(C.calloc(1, C.size_t(C.sizeof_VkMemoryAllocateInfo)))
	cAllocInfo.sType = C.VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO
	cAllocInfo.allocationSize = reqs.size
	cAllocInfo.memoryTypeIndex = C.uint32_t(memTypeIdx)
	cAllocInfo.pNext = unsafe.Pointer(cDedicatedInfo)

	res := C.vkAllocateMemory(ctx.Device, cAllocInfo, nil, &zc.mem)
	C.free(unsafe.Pointer(cAllocInfo))
	C.free(unsafe.Pointer(cDedicatedInfo))
	C.free(unsafe.Pointer(cExportInfo))
	if res != C.VK_SUCCESS {
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
	return zc.BlitFromWait(srcImage, layout, C.VK_QUEUE_FAMILY_IGNORED, nil)
}

// BlitFromWait is the same as BlitFrom, but optionally waits on core-provided
// semaphores before executing the copy. This is the minimal sync wiring needed
// for negotiated-device paths where the core may render the frame on a queue
// submission that has not yet become visible to our consumer copy.
func (zc *ZeroCopyBuffer) BlitFromWait(srcImage C.VkImage, layout C.VkImageLayout, srcQueueFamily C.uint32_t, waitSems []C.VkSemaphore) error {
	cmd, err := zc.ctx.beginOneShot()
	if err != nil {
		return err
	}
	C.blit_image_to_buffer(
		cmd,
		srcImage,
		layout,
		zc.buf,
		C.uint32_t(zc.width),
		C.uint32_t(zc.height),
		srcQueueFamily,
		C.uint32_t(zc.ctx.QueueFamily),
	)
	if len(waitSems) == 0 {
		return zc.ctx.submitOneShot(cmd)
	}
	res := C.cloudplay_submit_one_shot_wait_sems(
		zc.ctx.Device,
		zc.ctx.Queue,
		zc.ctx.CmdPool,
		cmd,
		C.uint32_t(len(waitSems)),
		(*C.VkSemaphore)(unsafe.Pointer(&waitSems[0])),
	)
	if res != C.VK_SUCCESS {
		return fmt.Errorf("zerocopy: vkQueueSubmit (wait sems): %d", int(res))
	}
	return nil
}

// BlitFromAsync submits the blit asynchronously with a fence.
// Returns immediately — the GPU work is in-flight.
// Call WaitBlit() before reading from the buffer.
//
// Safe to call repeatedly: if a previous async blit is pending,
// it waits for that fence first before submitting the new one.
func (zc *ZeroCopyBuffer) BlitFromAsync(srcImage C.VkImage, layout C.VkImageLayout, srcQueueFamily C.uint32_t) error {
	// Lazy-init fence and command buffer on first call.
	if zc.blitFence == nil {
		fenceInfo := C.VkFenceCreateInfo{
			sType: C.VK_STRUCTURE_TYPE_FENCE_CREATE_INFO,
		}
		if res := C.vkCreateFence(zc.ctx.Device, &fenceInfo, nil, &zc.blitFence); res != C.VK_SUCCESS {
			return fmt.Errorf("zerocopy: vkCreateFence: %d", int(res))
		}
	}

	// If a previous blit is pending, wait for it first.
	if zc.blitPending {
		C.vkWaitForFences(zc.ctx.Device, 1, &zc.blitFence, C.VK_TRUE, 5000000000) // 5s timeout
		zc.blitPending = false
	}

	// Free previous command buffer if it exists.
	if zc.blitCmd != nil {
		C.vkFreeCommandBuffers(zc.ctx.Device, zc.ctx.CmdPool, 1, &zc.blitCmd)
		zc.blitCmd = nil
	}

	// Allocate and record new command buffer.
	cmd, err := zc.ctx.beginOneShot()
	if err != nil {
		return err
	}
	C.blit_image_to_buffer(
		cmd,
		srcImage,
		layout,
		zc.buf,
		C.uint32_t(zc.width),
		C.uint32_t(zc.height),
		srcQueueFamily,
		C.uint32_t(zc.ctx.QueueFamily),
	)
	C.vkEndCommandBuffer(cmd)

	// Reset fence and submit.
	C.vkResetFences(zc.ctx.Device, 1, &zc.blitFence)

	// Submit via C helper to avoid Go pointer-in-struct CGo rule.
	res := C.cloudplay_submit_with_fence(zc.ctx.Queue, cmd, zc.blitFence)
	if res != C.VK_SUCCESS {
		C.vkFreeCommandBuffers(zc.ctx.Device, zc.ctx.CmdPool, 1, &cmd)
		return fmt.Errorf("zerocopy: vkQueueSubmit (async): %d", int(res))
	}

	zc.blitCmd = cmd
	zc.blitPending = true
	return nil
}

// WaitBlit waits for the most recent async blit to complete.
// No-op if no blit is pending.
// After this returns, the buffer contains the latest frame's RGBA8 data.
func (zc *ZeroCopyBuffer) WaitBlit() error {
	if !zc.blitPending {
		return nil
	}
	res := C.vkWaitForFences(zc.ctx.Device, 1, &zc.blitFence, C.VK_TRUE, 5000000000) // 5s timeout
	zc.blitPending = false
	if res != C.VK_SUCCESS {
		return fmt.Errorf("zerocopy: vkWaitForFences (blit): %d", int(res))
	}
	return nil
}

// ExportMemoryFd returns a fresh Linux file descriptor for the buffer's device
// memory via vkGetMemoryFdKHR.
//
// Important: for CUDA opaque-FD import on Linux, ownership of the fd is
// transferred to the CUDA driver on successful cuImportExternalMemory.
// Therefore we must not cache and reuse the same fd across retries/sessions.
// Each import attempt needs a newly exported fd for the same VkDeviceMemory.
func (zc *ZeroCopyBuffer) ExportMemoryFd() (int, error) {
	fd := int(C.export_memory_fd(zc.ctx.Device, zc.mem))
	if fd < 0 {
		return -1, fmt.Errorf("zerocopy: vkGetMemoryFdKHR failed")
	}
	return fd, nil
}

// ProbePrefix copies the first n bytes of the export buffer into a temporary
// host-visible staging buffer and returns a Go-owned copy. Diagnostic-only.
func (zc *ZeroCopyBuffer) ProbePrefix(n uint32) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	size := C.VkDeviceSize(n)

	bufInfo := C.VkBufferCreateInfo{
		sType: C.VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
		size:  size,
		usage: C.VK_BUFFER_USAGE_TRANSFER_DST_BIT,
	}
	var stageBuf C.VkBuffer
	if res := C.vkCreateBuffer(zc.ctx.Device, &bufInfo, nil, &stageBuf); res != C.VK_SUCCESS {
		return nil, fmt.Errorf("zerocopy: ProbePrefix vkCreateBuffer: %d", int(res))
	}
	defer C.vkDestroyBuffer(zc.ctx.Device, stageBuf, nil)

	var reqs C.VkMemoryRequirements
	C.vkGetBufferMemoryRequirements(zc.ctx.Device, stageBuf, &reqs)
	mem, err := zc.ctx.allocateMemory(reqs,
		C.VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT|C.VK_MEMORY_PROPERTY_HOST_COHERENT_BIT)
	if err != nil {
		return nil, fmt.Errorf("zerocopy: ProbePrefix allocateMemory: %w", err)
	}
	defer C.vkFreeMemory(zc.ctx.Device, mem, nil)
	if res := C.vkBindBufferMemory(zc.ctx.Device, stageBuf, mem, 0); res != C.VK_SUCCESS {
		return nil, fmt.Errorf("zerocopy: ProbePrefix vkBindBufferMemory: %d", int(res))
	}

	cmd, err := zc.ctx.beginOneShot()
	if err != nil {
		return nil, err
	}
	region := C.VkBufferCopy{srcOffset: 0, dstOffset: 0, size: size}
	C.vkCmdCopyBuffer(cmd, zc.buf, stageBuf, 1, &region)
	if err := zc.ctx.submitOneShot(cmd); err != nil {
		return nil, err
	}

	var mapped unsafe.Pointer
	if res := C.vkMapMemory(zc.ctx.Device, mem, 0, size, 0, &mapped); res != C.VK_SUCCESS {
		return nil, fmt.Errorf("zerocopy: ProbePrefix vkMapMemory: %d", int(res))
	}
	defer C.vkUnmapMemory(zc.ctx.Device, mem)

	out := make([]byte, int(size))
	copy(out, unsafe.Slice((*byte)(mapped), int(size)))
	return out, nil
}

// Size returns the buffer allocation size in bytes.
func (zc *ZeroCopyBuffer) Size() uint64 { return uint64(zc.size) }

// Destroy frees the Vulkan buffer and memory.
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

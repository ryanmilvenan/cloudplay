//go:build vulkan

package vulkan

/*
#cgo LDFLAGS: -lvulkan
#include <vulkan/vulkan.h>
#include <stdlib.h>

// Inline helper: insert an image layout transition via pipeline barrier.
static void transition_image_layout(
    VkCommandBuffer cmd,
    VkImage image,
    VkImageLayout oldLayout,
    VkImageLayout newLayout,
    VkAccessFlags srcAccess,
    VkAccessFlags dstAccess,
    VkPipelineStageFlags srcStage,
    VkPipelineStageFlags dstStage)
{
    VkImageMemoryBarrier barrier = {0};
    barrier.sType               = VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER;
    barrier.oldLayout           = oldLayout;
    barrier.newLayout           = newLayout;
    barrier.srcQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED;
    barrier.dstQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED;
    barrier.image               = image;
    barrier.subresourceRange.aspectMask     = VK_IMAGE_ASPECT_COLOR_BIT;
    barrier.subresourceRange.baseMipLevel   = 0;
    barrier.subresourceRange.levelCount     = 1;
    barrier.subresourceRange.baseArrayLayer = 0;
    barrier.subresourceRange.layerCount     = 1;
    barrier.srcAccessMask = srcAccess;
    barrier.dstAccessMask = dstAccess;

    vkCmdPipelineBarrier(
        cmd,
        srcStage, dstStage,
        0, 0, NULL, 0, NULL, 1, &barrier);
}
*/
import "C"
import (
	"fmt"
	"unsafe"
)

// FrameCapture manages a host-visible staging buffer used to read rendered
// frames from GPU (VkImage) to CPU memory for encoding.
type FrameCapture struct {
	ctx    *Context
	buf    C.VkBuffer
	mem    C.VkDeviceMemory
	size   C.VkDeviceSize
	width  uint32
	height uint32
	mapped unsafe.Pointer // persistently mapped pointer
}

// NewFrameCapture creates a staging buffer large enough for w×h RGBA8 pixels.
func NewFrameCapture(ctx *Context, w, h uint32) (*FrameCapture, error) {
	size := C.VkDeviceSize(w * h * 4) // 4 bytes per RGBA pixel
	fc := &FrameCapture{ctx: ctx, size: size, width: w, height: h}

	// Create staging buffer — host visible, coherent so we don't need explicit
	// cache flushes.
	bufInfo := C.VkBufferCreateInfo{
		sType: C.VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
		size:  size,
		usage: C.VK_BUFFER_USAGE_TRANSFER_DST_BIT,
	}
	if res := C.vkCreateBuffer(ctx.Device, &bufInfo, nil, &fc.buf); res != C.VK_SUCCESS {
		return nil, fmt.Errorf("vulkan: framecapture: vkCreateBuffer: %d", int(res))
	}

	var reqs C.VkMemoryRequirements
	C.vkGetBufferMemoryRequirements(ctx.Device, fc.buf, &reqs)

	mem, err := ctx.allocateMemory(reqs,
		C.VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT|C.VK_MEMORY_PROPERTY_HOST_COHERENT_BIT)
	if err != nil {
		C.vkDestroyBuffer(ctx.Device, fc.buf, nil)
		return nil, fmt.Errorf("vulkan: framecapture: %w", err)
	}
	fc.mem = mem

	if res := C.vkBindBufferMemory(ctx.Device, fc.buf, mem, 0); res != C.VK_SUCCESS {
		C.vkFreeMemory(ctx.Device, mem, nil)
		C.vkDestroyBuffer(ctx.Device, fc.buf, nil)
		return nil, fmt.Errorf("vulkan: framecapture: vkBindBufferMemory: %d", int(res))
	}

	// Persistently map for repeated readbacks.
	if res := C.vkMapMemory(ctx.Device, mem, 0, size, 0, &fc.mapped); res != C.VK_SUCCESS {
		C.vkFreeMemory(ctx.Device, mem, nil)
		C.vkDestroyBuffer(ctx.Device, fc.buf, nil)
		return nil, fmt.Errorf("vulkan: framecapture: vkMapMemory: %d", int(res))
	}

	return fc, nil
}

// Destroy unmaps and frees the staging buffer.
func (fc *FrameCapture) Destroy() {
	if fc.mapped != nil {
		C.vkUnmapMemory(fc.ctx.Device, fc.mem)
		fc.mapped = nil
	}
	if fc.buf != nil {
		C.vkDestroyBuffer(fc.ctx.Device, fc.buf, nil)
		fc.buf = nil
	}
	if fc.mem != nil {
		C.vkFreeMemory(fc.ctx.Device, fc.mem, nil)
		fc.mem = nil
	}
}

// Readback copies srcImage (which must be in VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL
// or will be transitioned from VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL) to the
// staging buffer and returns the pixel bytes as a Go slice.
//
// The returned slice is a view into the persistently-mapped buffer; copy it
// before the next call if you need to retain the data.
//
// Waits for the queue to be idle before submitting the copy command to ensure
// the core's rendering has completed before we read back the pixels.
func (fc *FrameCapture) Readback(srcImage C.VkImage, currentLayout C.VkImageLayout) ([]byte, error) {
	// Wait for any in-flight rendering to complete before reading back.
	// This ensures we see the completed frame, not partial rendering.
	if res := C.vkQueueWaitIdle(fc.ctx.Queue); res != C.VK_SUCCESS {
		return nil, fmt.Errorf("vulkan: framecapture: vkQueueWaitIdle: %d", int(res))
	}

	cmd, err := fc.ctx.beginOneShot()
	if err != nil {
		return nil, err
	}

	// 1. Transition image to TRANSFER_SRC_OPTIMAL if needed.
	if currentLayout != C.VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL {
		C.transition_image_layout(
			cmd, srcImage,
			currentLayout,
			C.VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL,
			C.VK_ACCESS_SHADER_WRITE_BIT,
			C.VK_ACCESS_TRANSFER_READ_BIT,
			C.VK_PIPELINE_STAGE_FRAGMENT_SHADER_BIT,
			C.VK_PIPELINE_STAGE_TRANSFER_BIT,
		)
	}

	// 2. Copy image → staging buffer.
	region := C.VkBufferImageCopy{
		bufferOffset:      0,
		bufferRowLength:   0, // tightly packed
		bufferImageHeight: 0,
		imageSubresource: C.VkImageSubresourceLayers{
			aspectMask:     C.VK_IMAGE_ASPECT_COLOR_BIT,
			mipLevel:       0,
			baseArrayLayer: 0,
			layerCount:     1,
		},
		imageOffset: C.VkOffset3D{x: 0, y: 0, z: 0},
		imageExtent: C.VkExtent3D{
			width:  C.uint32_t(fc.width),
			height: C.uint32_t(fc.height),
			depth:  1,
		},
	}
	C.vkCmdCopyImageToBuffer(cmd, srcImage, C.VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL, fc.buf, 1, &region)

	// 3. Transition image back to shader-read so the core can reuse it.
	C.transition_image_layout(
		cmd, srcImage,
		C.VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL,
		C.VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL,
		C.VK_ACCESS_TRANSFER_READ_BIT,
		C.VK_ACCESS_SHADER_READ_BIT,
		C.VK_PIPELINE_STAGE_TRANSFER_BIT,
		C.VK_PIPELINE_STAGE_FRAGMENT_SHADER_BIT,
	)

	if err := fc.ctx.submitOneShot(cmd); err != nil {
		return nil, err
	}

	// Return a Go slice backed by the mapped memory.
	size := int(fc.size)
	return (*[1 << 30]byte)(fc.mapped)[:size:size], nil
}

// ReadbackDirect copies from an image that is already in
// VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL (e.g. as set by the core via
// retro_vulkan_image).
func (fc *FrameCapture) ReadbackDirect(srcImage C.VkImage) ([]byte, error) {
	return fc.Readback(srcImage, C.VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL)
}

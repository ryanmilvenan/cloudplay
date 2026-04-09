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
// Ordering is guaranteed by the pipeline barrier in the command buffer (see
// transition_image_layout below) — no queue-wide stall needed here.
func (fc *FrameCapture) Readback(srcImage C.VkImage, currentLayout C.VkImageLayout) ([]byte, error) {
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

// ReadbackOwnTestImage creates a temporary VkImage, transitions it, copies
// to the staging buffer, and destroys it.  Used to test whether vkCmdCopyImageToBuffer
// works at all from our command pool.
func (fc *FrameCapture) ReadbackOwnTestImage() ([]byte, error) {
	// Create a small 64x64 test image
	imgInfo := C.VkImageCreateInfo{
		sType:     C.VK_STRUCTURE_TYPE_IMAGE_CREATE_INFO,
		imageType: C.VK_IMAGE_TYPE_2D,
		format:    C.VK_FORMAT_B8G8R8A8_UNORM,
		extent:    C.VkExtent3D{width: 64, height: 64, depth: 1},
		mipLevels: 1, arrayLayers: 1,
		samples:      C.VK_SAMPLE_COUNT_1_BIT,
		tiling:       C.VK_IMAGE_TILING_OPTIMAL,
		usage:        C.VK_IMAGE_USAGE_TRANSFER_SRC_BIT | C.VK_IMAGE_USAGE_TRANSFER_DST_BIT,
		initialLayout: C.VK_IMAGE_LAYOUT_UNDEFINED,
	}
	var testImg C.VkImage
	if res := C.vkCreateImage(fc.ctx.Device, &imgInfo, nil, &testImg); res != C.VK_SUCCESS {
		return nil, fmt.Errorf("vkCreateImage test: %d", int(res))
	}
	defer C.vkDestroyImage(fc.ctx.Device, testImg, nil)

	var reqs C.VkMemoryRequirements
	C.vkGetImageMemoryRequirements(fc.ctx.Device, testImg, &reqs)

	mem, err := fc.ctx.allocateMemory(reqs, C.VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT)
	if err != nil {
		return nil, fmt.Errorf("alloc test: %w", err)
	}
	defer C.vkFreeMemory(fc.ctx.Device, mem, nil)

	if res := C.vkBindImageMemory(fc.ctx.Device, testImg, mem, 0); res != C.VK_SUCCESS {
		return nil, fmt.Errorf("bindImage test: %d", int(res))
	}

	cmd, err := fc.ctx.beginOneShot()
	if err != nil {
		return nil, err
	}

	// Transition test image to TRANSFER_SRC
	C.transition_image_layout(cmd, testImg,
		C.VK_IMAGE_LAYOUT_UNDEFINED, C.VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL,
		0, C.VK_ACCESS_TRANSFER_READ_BIT,
		C.VK_PIPELINE_STAGE_TOP_OF_PIPE_BIT, C.VK_PIPELINE_STAGE_TRANSFER_BIT)

	region := C.VkBufferImageCopy{
		imageSubresource: C.VkImageSubresourceLayers{
			aspectMask: C.VK_IMAGE_ASPECT_COLOR_BIT, layerCount: 1,
		},
		imageExtent: C.VkExtent3D{width: 64, height: 64, depth: 1},
	}
	C.vkCmdCopyImageToBuffer(cmd, testImg, C.VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL, fc.buf, 1, &region)

	if err := fc.ctx.submitOneShot(cmd); err != nil {
		return nil, err
	}

	size := 64 * 64 * 4
	return C.GoBytes(fc.mapped, C.int(size)), nil
}

// TinyCopy submits a command buffer that copies just 1x1 pixel from the image.
// Used for diagnostics to isolate whether the problem is image dimensions.
func (fc *FrameCapture) TinyCopy(srcImage C.VkImage) error {
	cmd, err := fc.ctx.beginOneShot()
	if err != nil {
		return err
	}

	C.transition_image_layout(cmd, srcImage,
		C.VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL, C.VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL,
		C.VK_ACCESS_SHADER_READ_BIT, C.VK_ACCESS_TRANSFER_READ_BIT,
		C.VK_PIPELINE_STAGE_FRAGMENT_SHADER_BIT, C.VK_PIPELINE_STAGE_TRANSFER_BIT)

	region := C.VkBufferImageCopy{
		imageSubresource: C.VkImageSubresourceLayers{
			aspectMask: C.VK_IMAGE_ASPECT_COLOR_BIT, layerCount: 1,
		},
		imageExtent: C.VkExtent3D{width: 1, height: 1, depth: 1},
	}
	C.vkCmdCopyImageToBuffer(cmd, srcImage, C.VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL, fc.buf, 1, &region)

	C.transition_image_layout(cmd, srcImage,
		C.VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL, C.VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL,
		C.VK_ACCESS_TRANSFER_READ_BIT, C.VK_ACCESS_SHADER_READ_BIT,
		C.VK_PIPELINE_STAGE_TRANSFER_BIT, C.VK_PIPELINE_STAGE_FRAGMENT_SHADER_BIT)

	return fc.ctx.submitOneShot(cmd)
}

// TinyCopySize copies w×h pixels from the image. Used for diagnostic probing.
func (fc *FrameCapture) TinyCopySize(srcImage C.VkImage, w, h uint32) error {
	cmd, err := fc.ctx.beginOneShot()
	if err != nil {
		return err
	}

	C.transition_image_layout(cmd, srcImage,
		C.VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL, C.VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL,
		C.VK_ACCESS_SHADER_READ_BIT, C.VK_ACCESS_TRANSFER_READ_BIT,
		C.VK_PIPELINE_STAGE_FRAGMENT_SHADER_BIT, C.VK_PIPELINE_STAGE_TRANSFER_BIT)

	region := C.VkBufferImageCopy{
		imageSubresource: C.VkImageSubresourceLayers{
			aspectMask: C.VK_IMAGE_ASPECT_COLOR_BIT, layerCount: 1,
		},
		imageExtent: C.VkExtent3D{width: C.uint32_t(w), height: C.uint32_t(h), depth: 1},
	}
	C.vkCmdCopyImageToBuffer(cmd, srcImage, C.VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL, fc.buf, 1, &region)

	C.transition_image_layout(cmd, srcImage,
		C.VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL, C.VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL,
		C.VK_ACCESS_TRANSFER_READ_BIT, C.VK_ACCESS_SHADER_READ_BIT,
		C.VK_PIPELINE_STAGE_TRANSFER_BIT, C.VK_PIPELINE_STAGE_FRAGMENT_SHADER_BIT)

	return fc.ctx.submitOneShot(cmd)
}

// BarrierOnly submits a command buffer that contains just a pipeline barrier
// referencing the image (same layout → same layout, no actual work).
// Used for diagnostics: if this causes DEVICE_LOST, the issue is with
// referencing the image from our command buffer at all.
// cmd must be from beginOneShot (already begun).
func (fc *FrameCapture) BarrierOnly(cmd C.VkCommandBuffer, srcImage C.VkImage) error {
	C.transition_image_layout(
		cmd, srcImage,
		C.VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL,
		C.VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL,
		C.VK_ACCESS_SHADER_READ_BIT,
		C.VK_ACCESS_SHADER_READ_BIT,
		C.VK_PIPELINE_STAGE_FRAGMENT_SHADER_BIT,
		C.VK_PIPELINE_STAGE_FRAGMENT_SHADER_BIT,
	)
	return fc.ctx.submitOneShot(cmd)
}

// ReadbackGeneral copies using VK_IMAGE_LAYOUT_GENERAL for the source image.
// This is a workaround for cores like Dolphin where transitioning the image
// layout from SHADER_READ_ONLY causes DEVICE_LOST.  GENERAL layout is
// compatible with both shader reads and transfers per the Vulkan spec.
func (fc *FrameCapture) ReadbackGeneral(srcImage C.VkImage) ([]byte, error) {
	cmd, err := fc.ctx.beginOneShot()
	if err != nil {
		return nil, err
	}

	// Transition to GENERAL (compatible with both read and transfer)
	C.transition_image_layout(
		cmd, srcImage,
		C.VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL,
		C.VK_IMAGE_LAYOUT_GENERAL,
		C.VK_ACCESS_SHADER_READ_BIT,
		C.VK_ACCESS_TRANSFER_READ_BIT,
		C.VK_PIPELINE_STAGE_FRAGMENT_SHADER_BIT,
		C.VK_PIPELINE_STAGE_TRANSFER_BIT,
	)

	// Copy using GENERAL layout
	region := C.VkBufferImageCopy{
		bufferOffset:      0,
		bufferRowLength:   0,
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
	C.vkCmdCopyImageToBuffer(cmd, srcImage, C.VK_IMAGE_LAYOUT_GENERAL, fc.buf, 1, &region)

	// Transition back
	C.transition_image_layout(
		cmd, srcImage,
		C.VK_IMAGE_LAYOUT_GENERAL,
		C.VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL,
		C.VK_ACCESS_TRANSFER_READ_BIT,
		C.VK_ACCESS_SHADER_READ_BIT,
		C.VK_PIPELINE_STAGE_TRANSFER_BIT,
		C.VK_PIPELINE_STAGE_FRAGMENT_SHADER_BIT,
	)

	if err := fc.ctx.submitOneShot(cmd); err != nil {
		return nil, err
	}

	size := int(fc.size)
	return (*[1 << 30]byte)(fc.mapped)[:size:size], nil
}

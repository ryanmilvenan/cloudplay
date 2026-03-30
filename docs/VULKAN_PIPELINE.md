# CloudPlay Vulkan Rendering Pipeline

## Overview

Replace the current SDL/OpenGL/Xvfb video pipeline with a headless Vulkan rendering pipeline
and hardware-accelerated NVENC encoding. This eliminates the X server dependency and enables
zero-copy GPU frame capture → encode.

## Current Pipeline (what we're replacing)

```
Core renders → GL FBO → glReadPixels (GPU→CPU) → libyuv RGB→YUV (CPU) → x264 encode (CPU) → WebRTC
```

Problems:
- Requires X server (Xvfb or Xorg) for GLX context
- GL context is thread-bound (causes crashes with multi-threaded cores)
- glReadPixels is a blocking GPU→CPU copy (~2-5ms at 1080p)
- x264 software encoding uses significant CPU
- libyuv colorspace conversion on CPU

## New Pipeline

```
Core renders → VkImage (GPU) → NVENC input surface (zero-copy or fast GPU copy) → H264 NALUs → WebRTC
```

Benefits:
- No X server needed (Vulkan is headless-native)
- No thread-bound context (Vulkan handles are thread-safe with proper sync)
- No GPU→CPU readback for encoding
- NVENC hardware encoding (~1-2ms, ~5% GPU utilization)
- Async shader compilation works naturally in Vulkan

## Architecture

### 1. Vulkan Context Provider (`pkg/worker/caged/libretro/graphics/vulkan/`)

Implements the libretro `RETRO_HW_CONTEXT_VULKAN` interface:

```go
type VulkanContext struct {
    instance       vk.Instance
    physicalDevice vk.PhysicalDevice
    device         vk.Device
    queue          vk.Queue
    queueFamily    uint32
    renderImage    vk.Image       // core renders here
    renderMemory   vk.DeviceMemory
    commandPool    vk.CommandPool
}
```

Negotiation flow:
1. Core calls `SET_HW_RENDER` with `RETRO_HW_CONTEXT_VULKAN`
2. Frontend responds via `RETRO_ENVIRONMENT_SET_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_TYPE`
3. Frontend provides `retro_vulkan_context` with instance, device, queue
4. Core renders to provided VkImage
5. Frontend reads the rendered frame via `get_image` callback

Key interface from libretro:
```c
struct retro_hw_render_interface_vulkan {
    retro_vulkan_set_image_t        set_image;
    retro_vulkan_get_sync_index_t   get_sync_index;
    retro_vulkan_get_sync_index_mask_t get_sync_index_mask;
    retro_vulkan_set_command_buffers_t set_command_buffers;
    retro_vulkan_wait_sync_index_t  wait_sync_index;
    retro_vulkan_lock_queue_t       lock_queue;
    retro_vulkan_unlock_queue_t     unlock_queue;
    retro_vulkan_set_signal_semaphore_t set_signal_semaphore;
};
```

### 2. NVENC Encoder (`pkg/encoder/nvenc/`)

Uses NVIDIA's Video Codec SDK for hardware H264/HEVC encoding.

Options for integration:
- **FFmpeg's `h264_nvenc`** — easiest, via CGo to libavcodec
- **Direct NVIDIA Encoder API** — more control, lower latency
- **CUDA + NVENC** — zero-copy from Vulkan via CUDA external memory

Recommended: FFmpeg `h264_nvenc` for initial implementation, with option to go direct later.

```go
type NVENCEncoder struct {
    avCodecCtx  *C.AVCodecContext
    avFrame     *C.AVFrame
    avPacket    *C.AVPacket
    hwDeviceCtx *C.AVBufferRef
    hwFrameCtx  *C.AVBufferRef
}
```

### 3. Frame Transfer (Vulkan → NVENC)

Two approaches:

**A. Vulkan External Memory → CUDA → NVENC (zero-copy)**
```
VkImage → VK_KHR_external_memory → CUDA import → NV12 surface → NVENC
```
True zero-copy but requires CUDA interop setup.

**B. Vulkan → CPU readback → NVENC (simpler)**
```
VkImage → vkCmdCopyImageToBuffer → map → NV12 convert → NVENC upload
```
Still faster than GL readback because Vulkan copy can be async.

Start with B, optimize to A later.

### 4. Fallback Path

Keep the existing GL+x264 path as a fallback for:
- Systems without Vulkan
- Systems without NVENC (AMD GPUs, etc.)
- Cores that only support GL (some libretro cores)

Selection at startup:
```go
if hasVulkan && hasNVENC {
    pipeline = NewVulkanNVENCPipeline()
} else if hasGL {
    pipeline = NewGLSoftwarePipeline() // existing
}
```

## File Structure

```
pkg/
  worker/caged/libretro/
    graphics/
      vulkan/
        context.go       -- Vulkan instance/device creation
        provider.go      -- libretro HW render interface
        framecapture.go  -- VkImage readback
        vulkan.go        -- public API
      pipeline.go        -- abstract pipeline interface
      gl/                -- existing GL code (fallback)
      sdl.go             -- existing SDL code (fallback)
  encoder/
    nvenc/
      nvenc.go           -- NVENC encoder via FFmpeg
      nvenc_test.go
    encoder.go           -- add NVENC to codec selection
```

## Implementation Phases

### Phase 1: NVENC Encoder (drop-in replacement for x264)
- Implement `pkg/encoder/nvenc/` using FFmpeg h264_nvenc
- Wire into existing encoder selection
- Test: same GL pipeline but hardware encoding
- **Result: CPU encoding load eliminated**

### Phase 2: Vulkan Context Provider
- Implement headless Vulkan context
- Implement libretro Vulkan HW render interface
- Frame readback via staging buffer
- **Result: No X server dependency**

### Phase 3: Zero-Copy Pipeline
- Vulkan external memory → CUDA → NVENC
- Eliminate CPU-side frame copies
- **Result: Minimal latency, minimal GPU overhead**

## Dependencies

- Vulkan SDK headers (vulkan/vulkan.h)
- NVIDIA driver 470+ (Vulkan + NVENC support)
- FFmpeg with `--enable-nvenc` (for Phase 1)
- NVIDIA Video Codec SDK headers (for direct NVENC, Phase 3)

## Notes

- The RTX 3060 on moon.local supports Vulkan 1.3, NVENC (gen 7), H264+HEVC
- Podman needs `--device nvidia.com/gpu=all` (already configured)
- No X server means no `--shm-size` hack needed for Dolphin (fastmem uses regular mmap, not shm, when no GL context forces X)
- Audio pipeline (Opus) unchanged — it's already good

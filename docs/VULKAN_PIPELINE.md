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

### Phase 3: Zero-Copy Pipeline ✅ (scaffold landed; CUDA interop TODO)
- Vulkan external memory → CUDA → NVENC
- Eliminate CPU-side frame copies
- **Result: Minimal latency, minimal GPU overhead**
- See **Phase 3 implementation status** section below for details.

## Phase 3 Implementation Status

### What is implemented (this commit)

```
pkg/worker/caged/libretro/graphics/vulkan/
  context.go              — extended to request VK_KHR_external_memory[_fd] at
                            device creation; graceful fallback if unsupported;
                            Context.ExternalMemoryEnabled flag
  context_extmem.go       — (linux+vulkan+nvenc) device ext name list
  context_extmem_stub.go  — (other platforms) returns nil
  zerocopy.go             — (linux+vulkan+nvenc) ZeroCopyBuffer: device-local
                            exportable VkBuffer, BlitFrom (GPU→GPU copy),
                            ExportMemoryFd (vkGetMemoryFdKHR)
  zerocopy_stub.go        — (other platforms) stub types + error returns
  provider.go             — ensureZeroCopy (lazy alloc), ReadFrameZeroCopy
                            (blit + fd export), ZeroCopyBuffer() accessor;
                            Phase 2 staging readback kept as fallback
  vulkan.go               — ZeroCopyFd(), IsZeroCopyAvailable() on VulkanContext

pkg/worker/caged/libretro/nanoarch/
  nanoarch_vulkan.go      — vulkanZeroCopyFd(), Nanoarch.IsZeroCopyAvailable()
  nanoarch_novulkan.go    — stubs for the above

pkg/encoder/nvenc/
  nvenc_cuda.go           — (nvenc+linux) ImportExternalMemory (CUDA fd import,
                            lifetime-tracked via ExtMemHandle),
                            cuda_rgba_to_nv12 (PTX BT.601 kernel, JIT-compiled),
                            NVENC.EncodeFromDevPtr (GPU-only encode hot path),
                            ReleaseExternalMemory (CUDA handle cleanup)
  nvenc_cuda_stub.go      — (other platforms) stub returns errors
```

### What remains (TODOs for Phase 3d+)

1. ~~**CUDA external-memory handle lifetime**~~ ✅ **Fixed in Phase 3c** —
   `ExtMemHandle` wraps `CUexternalMemory`; `ReleaseExternalMemory` is called
   on `WebrtcMediaPipe.Destroy()`.

2. ~~**RGBA→NV12 GPU conversion**~~ ✅ **Implemented in Phase 3c** —
   `nvenc_encode_devptr` now runs two embedded PTX kernels (`rgba_to_nv12_y`
   and `rgba_to_nv12_uv`) that implement BT.601 studio-swing integer
   arithmetic, matching the CPU libyuv path.  JIT-compiled at runtime via
   `cuModuleLoadData` — no nvcc at build time required.  Falls back to Phase 3b
   raw copy if JIT fails (stable stream, wrong colours).

3. **Command buffer injection** — `go_set_command_buffers` still ignores the
   core's command buffers.  For a truly pipelined zero-copy path, append the
   `vkCmdCopyImageToBuffer` blit directly into the core's render command stream
   rather than using a separate one-shot submission.

4. **Semaphore / synchronisation** — `go_set_signal_semaphore` is a no-op.
   Wire in a VkSemaphore so the host and core submission are properly ordered
   without the conservative `vkDeviceWaitIdle` / `vkQueueWaitIdle` calls.

5. **PTX kernel unit test** — add a test that instantiates the CUDA module
   (or mocks the CUDA driver) and validates Y/U/V values against known inputs.
   Currently the PTX is exercised only by end-to-end encode runs.

### What was done in Phase 3b (commits 8f7c2d2, 6c51e90)

6. **Config flag** ✅ — `encoder.video.zeroCopy` boolean added to `config.Video`
   (default: `false`).  Also exposed in `config.yaml` with full docs.

7. **Media/frontend wiring** ✅ — `pkg/worker/media/` now has:
   - `ZeroCopyVideoEncoder` interface
   - `WebrtcMediaPipe.SetZeroCopyEncoder` / `ZeroCopyActive` / `ProcessVideoZeroCopy`
   - `ProcessVideo` transparently routes through zero-copy first, falls back to CPU
   - `TryArmZeroCopy` (build-tag-safe) arms the path after `Init()` in the coordinator handler
   - `zerocopy_stub.go` for non-nvenc/non-vulkan builds

8. **nanoarch / frontend / Caged exposure** ✅ — `Nanoarch.ZeroCopyFd(w, h)` method
   added (vulkan + novulkan builds), `Frontend.IsZeroCopyAvailable()` and
   `Frontend.ZeroCopyFd()` wired, `Caged.IsZeroCopyAvailable()` and
   `Caged.ZeroCopyFd()` exposed to the coordinator.

9. **Coordinator wiring** ✅ — `HandleGameStart` now calls `TryArmZeroCopy` when
   `config.ZeroCopy && app.IsZeroCopyAvailable()`.  The zero-copy NVENC encoder
   is created lazily (CUDA fd import happens on first rendered frame).

The CPU readback path is fully preserved as fallback and is the default for all builds.

### What was done in Phase 3c (this pass)

10. **GPU RGBA→NV12 colour conversion** ✅ — Two PTX kernels embedded in
    `nvenc_cuda.go`:
    - `rgba_to_nv12_y`: one thread per pixel, BT.601 integer Y calculation
    - `rgba_to_nv12_uv`: one thread per 2x2 block, 2×2-averaged U/V with
      BT.601 integer chroma calculation and clamping
    Both use the same integer coefficients as libyuv `ARGBToI420`, so colours
    match the CPU fallback path.  JIT-compiled via `cuModuleLoadData` at first
    use — no nvcc required at build time.

11. **CUexternalMemory leak fixed** ✅ — `cuda_import_external_memory` now
    returns a `nvenc_extmem_handle` (C struct) wrapped in Go's `ExtMemHandle`.
    `ReleaseExternalMemory` destroys the `CUexternalMemory` handle properly.
    `WebrtcMediaPipe.Destroy()` now calls `lazyZeroCopyNVENC.Destroy()` to
    release both the CUDA handle and the NVENC encoder.

12. **ZeroCopyVideoEncoder.Destroy()** ✅ — Added to the interface; implemented
    in `lazyZeroCopyNVENC`; called by `WebrtcMediaPipe.Destroy()`.

### Build tags

| Tag combination            | Behaviour                                                       |
|----------------------------|-----------------------------------------------------------------|
| (default)                  | Phase 1 x264 + GL readback                                      |
| `nvenc`                    | Phase 1 NVENC + GL readback (CPU upload)                        |
| `vulkan`                   | Phase 2 Vulkan readback (CPU staging buffer)                    |
| `vulkan nvenc`             | Phase 2 Vulkan + NVENC; external-mem not enabled                |
| `vulkan nvenc` on Linux    | Phase 3c available; ext-mem + CUDA import + PTX kernel wired;   |
|                            | gated by `encoder.video.zeroCopy: true` in config.yaml;         |
|                            | colours now correct (BT.601 = libyuv parity); JIT fallback safe |

## Dependencies

- Vulkan SDK headers (vulkan/vulkan.h)
- NVIDIA driver 470+ (Vulkan + NVENC support)
- FFmpeg with `--enable-nvenc` (for Phase 1)
- CUDA toolkit headers + libcuda (for Phase 3 `nvenc_cuda.go`)

## Notes

- The RTX 3060 on moon.local supports Vulkan 1.3, NVENC (gen 7), H264+HEVC
- Podman needs `--device nvidia.com/gpu=all` (already configured)
- No X server means no `--shm-size` hack needed for Dolphin (fastmem uses regular mmap, not shm, when no GL context forces X)
- Audio pipeline (Opus) unchanged — it's already good

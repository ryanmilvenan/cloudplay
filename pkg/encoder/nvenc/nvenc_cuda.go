//go:build nvenc && linux

package nvenc

// Phase 3c: CUDA external-memory import + GPU RGBA→NV12 conversion + NVENC encode.
//
// This file provides the complete zero-copy encode path:
//
//   Vulkan ZeroCopyBuffer (fd)
//     → cuImportExternalMemory (CUexternalMemory, lifetime-tracked)
//     → cuExternalMemoryGetMappedBuffer (CUdeviceptr, RGBA8 device memory)
//     → cuda_rgba_to_nv12 (PTX kernel: correct GPU colour conversion)
//     → nvenc_encode_devptr (NVENC reads NV12 surface directly)
//
// Colour correctness (Phase 3c):
//   The PTX kernel uses BT.601 studio-swing integer coefficients identical
//   to libyuv's ARGBToI420, so colours match the CPU fallback path exactly
//   (within rounding).  Specifically:
//     Y  = ((66*R + 129*G + 25*B  + 128) >> 8) + 16
//     Cb = ((-38*R - 74*G + 112*B + 128) >> 8) + 128
//     Cr = ((112*R - 94*G - 18*B + 128) >> 8) + 128
//   UV is 2×2 average-downsampled (4:2:0 chroma sub-sampling).
//
// JIT compilation: the PTX is embedded as a string literal and compiled by the
// CUDA driver at first use via cuModuleLoadData — no nvcc/ptxas at build time.
// If JIT fails (very old driver), the path falls back to Phase 3b raw-copy
// (wrong colours, but stable stream).
//
// Build requirement: nvenc + linux tags (CUDA headers, NVIDIA Linux driver).

/*
#cgo pkg-config: libavcodec libavutil
#cgo LDFLAGS: -lcuda -lcudart
#include "nvenc_ctx.h"
#include <libavutil/opt.h>
#include <libavutil/hwcontext.h>
#include <libavutil/hwcontext_cuda.h>
#include <libavutil/pixfmt.h>
#include <cuda.h>
#include <cudaTypedefs.h>
#include <stdlib.h>
#include <string.h>

// ─────────────────────────────────────────────────────────────────────────────
// PTX kernels for RGBA8 → NV12 (BT.601 studio-swing, integer arithmetic)
//
// Two kernels:
//   rgba_to_nv12_y   — one thread per pixel, writes Y plane (full resolution)
//   rgba_to_nv12_uv  — one thread per 2x2 pixel block, writes NV12 UV plane
//
// Both kernels are sm_30 (Kepler+) compatible.  All .reg declarations come
// before any instructions per PTX ABI requirements.
// ─────────────────────────────────────────────────────────────────────────────

// Y-plane kernel: each thread handles one pixel.
// Params: src (u64), dst_y (u64), width (u32), height (u32)
static const char *RGBA_TO_NV12_Y_PTX =
".version 7.0\n"
".target sm_30\n"
".address_size 64\n"
"\n"
".visible .entry rgba_to_nv12_y(\n"
"    .param .u64 p_src,\n"
"    .param .u64 p_dst_y,\n"
"    .param .u32 p_width,\n"
"    .param .u32 p_height\n"
")\n"
"{\n"
"    .reg .pred  %p0, %p1, %p2;\n"
"    .reg .u64   %src, %dst_y, %addr;\n"
"    .reg .u32   %width, %height;\n"
"    .reg .u32   %bx, %by, %tx, %ty, %x, %y;\n"
"    .reg .u32   %off4, %off1;\n"
"    .reg .u32   %R, %G, %B, %Y;\n"
"\n"
"    ld.param.u64 %src,    [p_src];\n"
"    ld.param.u64 %dst_y,  [p_dst_y];\n"
"    ld.param.u32 %width,  [p_width];\n"
"    ld.param.u32 %height, [p_height];\n"
"\n"
"    mov.u32 %bx, %ctaid.x;\n"
"    mov.u32 %by, %ctaid.y;\n"
"    mov.u32 %tx, %ntid.x;\n"
"    mov.u32 %ty, %ntid.y;\n"
"    mad.lo.u32 %x, %bx, %tx, %tid.x;\n"
"    mad.lo.u32 %y, %by, %ty, %tid.y;\n"
"\n"
"    setp.ge.u32 %p0, %x, %width;\n"
"    setp.ge.u32 %p1, %y, %height;\n"
"    or.pred %p2, %p0, %p1;\n"
"    @%p2 bra DONE;\n"
"\n"
"    mad.lo.u32 %off1, %y, %width, %x;\n"
"    shl.b32    %off4, %off1, 2;\n"
"    cvt.u64.u32 %addr, %off4;\n"
"    add.u64    %addr, %src, %addr;\n"
"\n"
"    ld.global.u8 %R, [%addr];\n"
"    ld.global.u8 %G, [%addr+1];\n"
"    ld.global.u8 %B, [%addr+2];\n"
"\n"
"    mul.lo.u32  %Y, %R, 66;\n"
"    mad.lo.u32  %Y, %G, 129, %Y;\n"
"    mad.lo.u32  %Y, %B, 25,  %Y;\n"
"    add.u32     %Y, %Y, 128;\n"
"    shr.u32     %Y, %Y, 8;\n"
"    add.u32     %Y, %Y, 16;\n"
"\n"
"    cvt.u64.u32 %addr, %off1;\n"
"    add.u64    %addr, %dst_y, %addr;\n"
"    st.global.u8 [%addr], %Y;\n"
"\n"
"DONE:\n"
"    ret;\n"
"}\n";

// UV-plane kernel: each thread handles one 2x2 pixel block.
// Params: src (u64), dst_uv (u64), width (u32), height (u32)
static const char *RGBA_TO_NV12_UV_PTX =
".version 7.0\n"
".target sm_30\n"
".address_size 64\n"
"\n"
".visible .entry rgba_to_nv12_uv(\n"
"    .param .u64 p_src,\n"
"    .param .u64 p_dst_uv,\n"
"    .param .u32 p_width,\n"
"    .param .u32 p_height\n"
")\n"
"{\n"
"    .reg .pred  %p0, %p1, %p2;\n"
"    .reg .u64   %src, %dst_uv, %addr;\n"
"    .reg .u32   %width, %height, %hw, %hh;\n"
"    .reg .u32   %bx, %by, %bsx, %bsy, %bx2, %by2;\n"
"    .reg .u32   %cx, %cy;\n"
"    .reg .u32   %off, %off4;\n"
"    .reg .u32   %R0, %G0, %B0;\n"
"    .reg .u32   %R1, %G1, %B1;\n"
"    .reg .u32   %R2, %G2, %B2;\n"
"    .reg .u32   %R3, %G3, %B3;\n"
"    .reg .u32   %Ra, %Ga, %Ba;\n"
"    .reg .s32   %sRa, %sGa, %sBa;\n"
"    .reg .s32   %Cb, %Cr;\n"
"    .reg .u32   %wm1, %hm1;\n"
"    .reg .u32   %x0, %x1, %y0, %y1;\n"
"\n"
"    ld.param.u64 %src,    [p_src];\n"
"    ld.param.u64 %dst_uv, [p_dst_uv];\n"
"    ld.param.u32 %width,  [p_width];\n"
"    ld.param.u32 %height, [p_height];\n"
"\n"
"    shr.u32 %hw, %width,  1;\n"
"    shr.u32 %hh, %height, 1;\n"
"\n"
"    mov.u32 %bsx, %ntid.x;\n"
"    mov.u32 %bsy, %ntid.y;\n"
"    mov.u32 %bx,  %ctaid.x;\n"
"    mov.u32 %by,  %ctaid.y;\n"
"    mad.lo.u32 %bx2, %bx, %bsx, %tid.x;\n"
"    mad.lo.u32 %by2, %by, %bsy, %tid.y;\n"
"\n"
"    setp.ge.u32 %p0, %bx2, %hw;\n"
"    setp.ge.u32 %p1, %by2, %hh;\n"
"    or.pred %p2, %p0, %p1;\n"
"    @%p2 bra UVDONE;\n"
"\n"
"    shl.b32 %cx, %bx2, 1;\n"
"    shl.b32 %cy, %by2, 1;\n"
"\n"
"    sub.u32 %wm1, %width,  1;\n"
"    sub.u32 %hm1, %height, 1;\n"
"\n"
"    min.u32 %x0, %cx,      %wm1;\n"
"    min.u32 %x1, %cx,      %wm1;\n"
"    add.u32 %x1, %x1, 1;\n"
"    min.u32 %x1, %x1, %wm1;\n"
"    min.u32 %y0, %cy,      %hm1;\n"
"    min.u32 %y1, %cy, %hm1;\n"
"    add.u32 %y1, %y1, 1;\n"
"    min.u32 %y1, %y1, %hm1;\n"
"\n"
"    mad.lo.u32 %off, %y0, %width, %x0;\n"
"    shl.b32    %off4, %off, 2;\n"
"    cvt.u64.u32 %addr, %off4;\n"
"    add.u64    %addr, %src, %addr;\n"
"    ld.global.u8 %R0, [%addr];\n"
"    ld.global.u8 %G0, [%addr+1];\n"
"    ld.global.u8 %B0, [%addr+2];\n"
"\n"
"    mad.lo.u32 %off, %y0, %width, %x1;\n"
"    shl.b32    %off4, %off, 2;\n"
"    cvt.u64.u32 %addr, %off4;\n"
"    add.u64    %addr, %src, %addr;\n"
"    ld.global.u8 %R1, [%addr];\n"
"    ld.global.u8 %G1, [%addr+1];\n"
"    ld.global.u8 %B1, [%addr+2];\n"
"\n"
"    mad.lo.u32 %off, %y1, %width, %x0;\n"
"    shl.b32    %off4, %off, 2;\n"
"    cvt.u64.u32 %addr, %off4;\n"
"    add.u64    %addr, %src, %addr;\n"
"    ld.global.u8 %R2, [%addr];\n"
"    ld.global.u8 %G2, [%addr+1];\n"
"    ld.global.u8 %B2, [%addr+2];\n"
"\n"
"    mad.lo.u32 %off, %y1, %width, %x1;\n"
"    shl.b32    %off4, %off, 2;\n"
"    cvt.u64.u32 %addr, %off4;\n"
"    add.u64    %addr, %src, %addr;\n"
"    ld.global.u8 %R3, [%addr];\n"
"    ld.global.u8 %G3, [%addr+1];\n"
"    ld.global.u8 %B3, [%addr+2];\n"
"\n"
"    add.u32 %Ra, %R0, %R1;\n"
"    add.u32 %Ra, %Ra, %R2;\n"
"    add.u32 %Ra, %Ra, %R3;\n"
"    shr.u32 %Ra, %Ra, 2;\n"
"\n"
"    add.u32 %Ga, %G0, %G1;\n"
"    add.u32 %Ga, %Ga, %G2;\n"
"    add.u32 %Ga, %Ga, %G3;\n"
"    shr.u32 %Ga, %Ga, 2;\n"
"\n"
"    add.u32 %Ba, %B0, %B1;\n"
"    add.u32 %Ba, %Ba, %B2;\n"
"    add.u32 %Ba, %Ba, %B3;\n"
"    shr.u32 %Ba, %Ba, 2;\n"
"\n"
"    cvt.s32.u32 %sRa, %Ra;\n"
"    cvt.s32.u32 %sGa, %Ga;\n"
"    cvt.s32.u32 %sBa, %Ba;\n"
"\n"
"    mul.lo.s32  %Cb, %sRa, -38;\n"
"    mad.lo.s32  %Cb, %sGa, -74, %Cb;\n"
"    mad.lo.s32  %Cb, %sBa, 112, %Cb;\n"
"    add.s32     %Cb, %Cb, 128;\n"
"    shr.s32     %Cb, %Cb, 8;\n"
"    add.s32     %Cb, %Cb, 128;\n"
"    max.s32     %Cb, %Cb, 0;\n"
"    min.s32     %Cb, %Cb, 255;\n"
"\n"
"    mul.lo.s32  %Cr, %sRa, 112;\n"
"    mad.lo.s32  %Cr, %sGa, -94, %Cr;\n"
"    mad.lo.s32  %Cr, %sBa, -18, %Cr;\n"
"    add.s32     %Cr, %Cr, 128;\n"
"    shr.s32     %Cr, %Cr, 8;\n"
"    add.s32     %Cr, %Cr, 128;\n"
"    max.s32     %Cr, %Cr, 0;\n"
"    min.s32     %Cr, %Cr, 255;\n"
"\n"
"    mad.lo.u32 %off, %by2, %hw, %bx2;\n"
"    shl.b32    %off4, %off, 1;\n"
"    cvt.u64.u32 %addr, %off4;\n"
"    add.u64    %addr, %dst_uv, %addr;\n"
"    st.global.u8 [%addr],   %Cb;\n"
"    st.global.u8 [%addr+1], %Cr;\n"
"\n"
"UVDONE:\n"
"    ret;\n"
"}\n";

// ─────────────────────────────────────────────────────────────────────────────
// cuda_color_ctx: lazily initialised per-session state for PTX colour converter.
// ─────────────────────────────────────────────────────────────────────────────
typedef struct {
    CUmodule    mod_y;           // loaded PTX module for Y kernel
    CUmodule    mod_uv;          // loaded PTX module for UV kernel
    CUfunction  fn_y;            // rgba_to_nv12_y kernel function
    CUfunction  fn_uv;           // rgba_to_nv12_uv kernel function
    CUdeviceptr nv12_y;          // device buffer — Y plane (width*height)
    CUdeviceptr nv12_uv;         // device buffer — UV plane (width/2*height/2*2)
    int         width;
    int         height;
    int         ptx_ok;          // 1 if PTX compiled and kernels loaded OK
    int         initialised;     // 1 if buffers allocated
} cuda_color_ctx;

static cuda_color_ctx g_color_ctx = {0};

// cuda_color_release frees all resources in g_color_ctx.
static void cuda_color_release(void) {
    if (g_color_ctx.nv12_y)  { cuMemFree(g_color_ctx.nv12_y);  g_color_ctx.nv12_y  = 0; }
    if (g_color_ctx.nv12_uv) { cuMemFree(g_color_ctx.nv12_uv); g_color_ctx.nv12_uv = 0; }
    if (g_color_ctx.mod_y)   { cuModuleUnload(g_color_ctx.mod_y);  g_color_ctx.mod_y  = NULL; }
    if (g_color_ctx.mod_uv)  { cuModuleUnload(g_color_ctx.mod_uv); g_color_ctx.mod_uv = NULL; }
    g_color_ctx.fn_y = NULL;
    g_color_ctx.fn_uv = NULL;
    g_color_ctx.ptx_ok = 0;
    g_color_ctx.initialised = 0;
}

// cuda_color_init prepares the PTX modules, kernels, and NV12 output buffers.
// Idempotent: subsequent calls are no-ops unless dimensions changed.
// Returns 1 on success, 0 on failure (PTX JIT error or OOM).
static int cuda_color_init(int width, int height) {
    if (g_color_ctx.initialised &&
        g_color_ctx.width == width &&
        g_color_ctx.height == height) {
        return g_color_ctx.ptx_ok;
    }

    cuda_color_release();

    // JIT-compile the Y-plane PTX module.
    if (cuModuleLoadData(&g_color_ctx.mod_y, RGBA_TO_NV12_Y_PTX) != CUDA_SUCCESS)
        goto fail;
    if (cuModuleGetFunction(&g_color_ctx.fn_y, g_color_ctx.mod_y, "rgba_to_nv12_y") != CUDA_SUCCESS)
        goto fail;

    // JIT-compile the UV-plane PTX module.
    if (cuModuleLoadData(&g_color_ctx.mod_uv, RGBA_TO_NV12_UV_PTX) != CUDA_SUCCESS)
        goto fail;
    if (cuModuleGetFunction(&g_color_ctx.fn_uv, g_color_ctx.mod_uv, "rgba_to_nv12_uv") != CUDA_SUCCESS)
        goto fail;

    // Allocate NV12 output buffers.
    //   Y plane:   width * height bytes (one luma sample per pixel)
    //   UV plane:  (width/2) * (height/2) * 2 bytes (interleaved Cb Cr, half-res)
    {
        size_t y_size  = (size_t)width * height;
        size_t uv_size = (size_t)(width / 2) * (height / 2) * 2;
        if (cuMemAlloc(&g_color_ctx.nv12_y,  y_size)  != CUDA_SUCCESS) goto fail;
        if (cuMemAlloc(&g_color_ctx.nv12_uv, uv_size) != CUDA_SUCCESS) goto fail;
    }

    g_color_ctx.width       = width;
    g_color_ctx.height      = height;
    g_color_ctx.ptx_ok      = 1;
    g_color_ctx.initialised = 1;
    return 1;

fail:
    cuda_color_release();
    return 0;
}

// cuda_rgba_to_nv12 converts an RGBA8 device buffer to NV12 using PTX kernels.
//
// On success, g_color_ctx.nv12_y and g_color_ctx.nv12_uv hold the result.
// Returns 1 on success, 0 on failure (falls back to Phase 3b raw copy).
static int cuda_rgba_to_nv12(CUdeviceptr rgba_src, int width, int height) {
    if (!cuda_color_init(width, height))
        return 0;

    // Y kernel: 16x16 thread block, grid covers all pixels.
    {
        unsigned int bw = 16, bh = 16;
        unsigned int gw = ((unsigned int)width  + bw - 1) / bw;
        unsigned int gh = ((unsigned int)height + bh - 1) / bh;
        void *args[] = { &rgba_src, &g_color_ctx.nv12_y, &width, &height };
        if (cuLaunchKernel(g_color_ctx.fn_y, gw, gh, 1, bw, bh, 1, 0, NULL, args, NULL) != CUDA_SUCCESS)
            return 0;
    }

    // UV kernel: 16x16 thread block, grid covers half-resolution pixel blocks.
    {
        int hw = width  / 2;
        int hh = height / 2;
        unsigned int bw = 16, bh = 16;
        unsigned int gw = ((unsigned int)hw + bw - 1) / bw;
        unsigned int gh = ((unsigned int)hh + bh - 1) / bh;
        void *args[] = { &rgba_src, &g_color_ctx.nv12_uv, &width, &height };
        if (cuLaunchKernel(g_color_ctx.fn_uv, gw, gh, 1, bw, bh, 1, 0, NULL, args, NULL) != CUDA_SUCCESS)
            return 0;
    }

    // Synchronise: NVENC reads from nv12_y/nv12_uv immediately after this call.
    cuCtxSynchronize();
    return 1;
}

// ─────────────────────────────────────────────────────────────────────────────
// External memory import — with lifetime tracking (fixes Phase 3b leak).
// ─────────────────────────────────────────────────────────────────────────────

// nvenc_extmem_handle pairs a CUexternalMemory with its mapped device pointer.
typedef struct {
    CUexternalMemory extMem;
    CUdeviceptr      devPtr;
} nvenc_extmem_handle;

// cuda_import_external_memory imports a Linux opaque fd into CUDA.
// Returns a heap-allocated handle on success; NULL on failure.
// Caller must free with nvenc_release_extmem.
static nvenc_extmem_handle *cuda_import_external_memory(int fd, size_t size) {
    nvenc_extmem_handle *h = (nvenc_extmem_handle *)calloc(1, sizeof(*h));
    if (!h) return NULL;

    CUDA_EXTERNAL_MEMORY_HANDLE_DESC desc = {0};
    desc.type       = CU_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD;
    desc.handle.fd  = fd;
    desc.size       = size;

    if (cuImportExternalMemory(&h->extMem, &desc) != CUDA_SUCCESS) {
        free(h);
        return NULL;
    }

    CUDA_EXTERNAL_MEMORY_BUFFER_DESC bufDesc = {0};
    bufDesc.offset = 0;
    bufDesc.size   = size;

    if (cuExternalMemoryGetMappedBuffer(&h->devPtr, h->extMem, &bufDesc) != CUDA_SUCCESS) {
        cuDestroyExternalMemory(h->extMem);
        free(h);
        return NULL;
    }

    return h;
}

// nvenc_release_extmem destroys the CUDA external-memory handle.
static void nvenc_release_extmem(nvenc_extmem_handle *h) {
    if (!h) return;
    if (h->extMem) cuDestroyExternalMemory(h->extMem);
    free(h);
}

// ─────────────────────────────────────────────────────────────────────────────
// nvenc_encode_devptr: encode one RGBA8 frame from a CUDA device pointer.
//
// Phase 3c hot path:
//   1. Run PTX RGBA→NV12 kernels (BT.601 correct, GPU-only).
//   2. DtoD-copy NV12 planes into the NVENC hw_frame CUDA surface.
//   3. Submit to NVENC; receive H.264 NAL bytes.
//
// If PTX JIT/launch fails (fallback): raw cuMemcpyDtoD into Y plane only —
// stream stays up, colours are wrong. Matching Phase 3b behaviour so no
// regression relative to the previous commit.
// ─────────────────────────────────────────────────────────────────────────────
static uint8_t *nvenc_encode_devptr(nvenc_ctx *ctx, CUdeviceptr devPtr, size_t rgba_size, int *out_size) {
    *out_size = 0;
    if (!devPtr || !ctx) return NULL;

    AVFrame *hw_frame = av_frame_alloc();
    if (!hw_frame) return NULL;

    int ret = av_hwframe_get_buffer(ctx->codec_ctx->hw_frames_ctx, hw_frame, 0);
    if (ret < 0) {
        av_frame_free(&hw_frame);
        return NULL;
    }

    // Attempt GPU colour conversion via PTX kernels.
    int color_ok = cuda_rgba_to_nv12(devPtr, ctx->width, ctx->height);

    if (color_ok) {
        size_t y_size  = (size_t)ctx->width * ctx->height;
        size_t uv_size = (size_t)(ctx->width / 2) * (ctx->height / 2) * 2;

        CUdeviceptr hw_y  = (CUdeviceptr)(uintptr_t)hw_frame->data[0];
        CUdeviceptr hw_uv = (CUdeviceptr)(uintptr_t)hw_frame->data[1];

        if (cuMemcpyDtoD(hw_y,  g_color_ctx.nv12_y,  y_size)  != CUDA_SUCCESS ||
            cuMemcpyDtoD(hw_uv, g_color_ctx.nv12_uv, uv_size) != CUDA_SUCCESS) {
            color_ok = 0;
        }
    }

    if (!color_ok) {
        // Phase 3b fallback: raw copy of RGBA bytes into Y plane only.
        // Colours are wrong but stream stays stable.
        size_t copy_size = rgba_size < (size_t)(ctx->width * ctx->height * 4)
                         ? rgba_size : (size_t)(ctx->width * ctx->height * 4);
        cuMemcpyDtoD((CUdeviceptr)(uintptr_t)hw_frame->data[0], devPtr, copy_size);
    }

    hw_frame->pts = ctx->pts++;
    ret = avcodec_send_frame(ctx->codec_ctx, hw_frame);
    av_frame_free(&hw_frame);
    if (ret < 0) return NULL;

    av_packet_unref(ctx->packet);
    ret = avcodec_receive_packet(ctx->codec_ctx, ctx->packet);
    if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) return NULL;
    if (ret < 0) return NULL;

    *out_size = ctx->packet->size;
    return ctx->packet->data;
}
*/
import "C"
import (
	"fmt"
	"sync"
	"unsafe"
)

var extmemMu sync.Mutex

// ExtMemHandle tracks the CUDA external-memory handle for cleanup.
// Returned by ImportExternalMemory; pass to ReleaseExternalMemory on teardown.
type ExtMemHandle struct {
	h *C.nvenc_extmem_handle
}

// ImportExternalMemory imports a Linux opaque fd (from ZeroCopyBuffer.ExportMemoryFd)
// into CUDA and returns:
//   - cudaDevPtr: CUDA device pointer to the Vulkan device memory (RGBA8)
//   - handle: must be released via ReleaseExternalMemory when the buffer is freed
//   - err: non-nil on import failure
//
// Supersedes Phase 3b: the CUexternalMemory is now lifetime-tracked, fixing
// the previous leak.
func ImportExternalMemory(fd int, size uint64) (uintptr, *ExtMemHandle, error) {
	extmemMu.Lock()
	defer extmemMu.Unlock()

	h := C.cuda_import_external_memory(C.int(fd), C.size_t(size))
	if h == nil {
		return 0, nil, fmt.Errorf("nvenc: cuda_import_external_memory failed for fd=%d", fd)
	}
	return uintptr(h.devPtr), &ExtMemHandle{h: h}, nil
}

// ReleaseExternalMemory destroys the CUDA external-memory handle.
// Must be called when the underlying Vulkan ZeroCopyBuffer is destroyed.
func ReleaseExternalMemory(handle *ExtMemHandle) {
	if handle == nil || handle.h == nil {
		return
	}
	extmemMu.Lock()
	defer extmemMu.Unlock()
	C.nvenc_release_extmem(handle.h)
	handle.h = nil
}

// EncodeFromDevPtr encodes one frame from a CUDA device pointer (RGBA8).
//
// Phase 3c: runs the embedded PTX RGBA→NV12 kernel (BT.601 correct) before
// sending to NVENC.  If PTX JIT fails, degrades to Phase 3b raw-copy fallback
// (incorrect colours, stable stream).
//
// size must be width*height*4 (RGBA8 byte size).
// Returns nil+nil on EAGAIN (encoder buffering) — not an error.
func (e *NVENC) EncodeFromDevPtr(cudaDevPtr uintptr, size uint64) ([]byte, error) {
	if e.ctx == nil {
		return nil, fmt.Errorf("nvenc: encoder not initialised")
	}

	var outSize C.int
	data := C.nvenc_encode_devptr(
		e.ctx,
		C.CUdeviceptr(cudaDevPtr),
		C.size_t(size),
		&outSize,
	)
	if data == nil || outSize == 0 {
		return nil, nil
	}

	result := make([]byte, int(outSize))
	copy(result, unsafe.Slice((*byte)(unsafe.Pointer(data)), int(outSize)))
	return result, nil
}

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

// Debug toggle: force neutral chroma (U=128, V=128) while preserving PTX Y.
// If the stream becomes correct grayscale, the bug is in chroma layout/order.
// If it stays broken, suspect pitch/stride or plane addressing.
#define CLOUDPLAY_DIAG_FORCE_NEUTRAL_UV 0

// Debug toggle: bypass PTX/source content entirely and write a synthetic
// grayscale ramp directly into the destination NVENC hw frame using the exact
// destination pitches. If the browser still renders green, the remaining bug
// is in destination surface semantics / encoder interpretation rather than the
// PTX conversion path.
#define CLOUDPLAY_DIAG_DEST_PATTERN 0
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

// ─────────────────────────────────────────────────────────────────────────────
// PTX kernels for RGBA8 → NV12 (BT.601 studio-swing, integer arithmetic)
//
// Two kernels:
//   rgba_to_nv12_y   — one thread per pixel, writes Y plane (full resolution)
//   rgba_to_nv12_uv  — one thread per 2x2 pixel block, writes NV12 UV plane
//
// Both kernels target sm_52 (Maxwell+, minimum for CUDA 12.x JIT).  All .reg declarations come
// before any instructions per PTX ABI requirements.
// ─────────────────────────────────────────────────────────────────────────────

// Y-plane kernel: each thread handles one pixel.
// Params: src (u64), dst_y (u64), width (u32), height (u32)
static const char *RGBA_TO_NV12_Y_PTX =
".version 8.0\n"
".target sm_86\n"
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
"    .reg .u32   %bx, %by, %tx, %ty, %x, %y, %tidx, %tidy;\n"
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
"    mov.u32 %tidx, %tid.x;\n"
"    mov.u32 %tidy, %tid.y;\n"
"    mad.lo.u32 %x, %bx, %tx, %tidx;\n"
"    mad.lo.u32 %y, %by, %ty, %tidy;\n"
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
"    // Vulkan VK_FORMAT_B8G8R8A8_UNORM: byte order is B,G,R,A\n"
"    ld.global.u8 %B, [%addr];\n"
"    ld.global.u8 %G, [%addr+1];\n"
"    ld.global.u8 %R, [%addr+2];\n"
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
".version 8.0\n"
".target sm_86\n"
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
"    mov.u32 %bx2, %tid.x;\n"
"    mov.u32 %by2, %tid.y;\n"
"    mad.lo.u32 %bx2, %bx, %bsx, %bx2;\n"
"    mad.lo.u32 %by2, %by, %bsy, %by2;\n"
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
"    // BGRA byte order\n"
"    ld.global.u8 %B0, [%addr];\n"
"    ld.global.u8 %G0, [%addr+1];\n"
"    ld.global.u8 %R0, [%addr+2];\n"
"\n"
"    mad.lo.u32 %off, %y0, %width, %x1;\n"
"    shl.b32    %off4, %off, 2;\n"
"    cvt.u64.u32 %addr, %off4;\n"
"    add.u64    %addr, %src, %addr;\n"
"    ld.global.u8 %B1, [%addr];\n"
"    ld.global.u8 %G1, [%addr+1];\n"
"    ld.global.u8 %R1, [%addr+2];\n"
"\n"
"    mad.lo.u32 %off, %y1, %width, %x0;\n"
"    shl.b32    %off4, %off, 2;\n"
"    cvt.u64.u32 %addr, %off4;\n"
"    add.u64    %addr, %src, %addr;\n"
"    ld.global.u8 %B2, [%addr];\n"
"    ld.global.u8 %G2, [%addr+1];\n"
"    ld.global.u8 %R2, [%addr+2];\n"
"\n"
"    mad.lo.u32 %off, %y1, %width, %x1;\n"
"    shl.b32    %off4, %off, 2;\n"
"    cvt.u64.u32 %addr, %off4;\n"
"    add.u64    %addr, %src, %addr;\n"
"    ld.global.u8 %B3, [%addr];\n"
"    ld.global.u8 %G3, [%addr+1];\n"
"    ld.global.u8 %R3, [%addr+2];\n"
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

    CUresult res;
    // Check CUDA context state before PTX JIT
    {
        CUcontext cur_ctx = NULL;
        CUresult ctx_res = cuCtxGetCurrent(&cur_ctx);
        int driver_ver = 0;
        cuDriverGetVersion(&driver_ver);
        fprintf(stderr, "[cloudplay diag] cuda_color_init: cuCtxGetCurrent=%d cur_ctx=%p driver_ver=%d dims=%dx%d\n",
                (int)ctx_res, (void*)cur_ctx, driver_ver, width, height);
                fflush(stderr);
    }
    // Try a trivial PTX module first to test if cuModuleLoadData works at all
    {
        static const char *TRIVIAL_PTX = ".version 8.0\n.target sm_86\n.address_size 64\n.visible .entry test_noop() { ret; }\n";
        CUmodule test_mod = NULL;
        CUresult test_res = cuModuleLoadData(&test_mod, TRIVIAL_PTX);
        fprintf(stderr, "[cloudplay diag] cuda_color_init: trivial PTX test CUresult=%d\n", (int)test_res);
        fflush(stderr);
        if (test_mod) cuModuleUnload(test_mod);
        fflush(stderr);
    }
    // JIT-compile the Y-plane PTX module with error log.
    {
        char jit_log[4096] = {0};
        CUjit_option jit_opts[] = { CU_JIT_ERROR_LOG_BUFFER, CU_JIT_ERROR_LOG_BUFFER_SIZE_BYTES };
        void *jit_vals[] = { (void*)jit_log, (void*)(size_t)sizeof(jit_log) };
        res = cuModuleLoadDataEx(&g_color_ctx.mod_y, RGBA_TO_NV12_Y_PTX, 2, jit_opts, jit_vals);
        if (res != CUDA_SUCCESS) {
            fprintf(stderr, "[cloudplay diag] cuda_color_init: cuModuleLoadDataEx(Y) failed: CUresult=%d jit_log='%s'\n", (int)res, jit_log);
            fflush(stderr);
            goto fail;
        }
    }
    res = cuModuleGetFunction(&g_color_ctx.fn_y, g_color_ctx.mod_y, "rgba_to_nv12_y");
    if (res != CUDA_SUCCESS) {
        fprintf(stderr, "[cloudplay diag] cuda_color_init: cuModuleGetFunction(Y) failed: CUresult=%d\n", (int)res);
        fflush(stderr);
        goto fail;
    }

    // JIT-compile the UV-plane PTX module.
    res = cuModuleLoadData(&g_color_ctx.mod_uv, RGBA_TO_NV12_UV_PTX);
    if (res != CUDA_SUCCESS) {
        fprintf(stderr, "[cloudplay diag] cuda_color_init: cuModuleLoadData(UV) failed: CUresult=%d\n", (int)res);
        fflush(stderr);
        goto fail;
    }
    res = cuModuleGetFunction(&g_color_ctx.fn_uv, g_color_ctx.mod_uv, "rgba_to_nv12_uv");
    if (res != CUDA_SUCCESS) {
        fprintf(stderr, "[cloudplay diag] cuda_color_init: cuModuleGetFunction(UV) failed: CUresult=%d\n", (int)res);
        fflush(stderr);
        goto fail;
    }

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
    fprintf(stderr, "[cloudplay diag] cuda_color_init: SUCCESS\n"); fflush(stderr);
    fflush(stderr);
    return 1;

fail:
    fprintf(stderr, "[cloudplay diag] cuda_color_init: FAILED\n"); fflush(stderr);
    fflush(stderr);
    cuda_color_release();
    fflush(stderr);
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

#if CLOUDPLAY_DIAG_FORCE_NEUTRAL_UV
    {
        size_t uv_size = (size_t)width * (size_t)height / 2;
        if (cuMemsetD8(g_color_ctx.nv12_uv, 128, uv_size) != CUDA_SUCCESS)
            return 0;
    }
#else
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
#endif

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
static nvenc_extmem_handle *cuda_import_external_memory(CUcontext cu_ctx, int fd, size_t size, CUresult *out_import_err, CUresult *out_map_err) {
    nvenc_extmem_handle *h = (nvenc_extmem_handle *)calloc(1, sizeof(*h));
    if (!h) return NULL;

    if (out_import_err) *out_import_err = CUDA_SUCCESS;
    if (out_map_err) *out_map_err = CUDA_SUCCESS;

    CUcontext prev = NULL;
    if (cu_ctx && cuCtxPushCurrent(cu_ctx) != CUDA_SUCCESS) {
        if (out_import_err) *out_import_err = CUDA_ERROR_INVALID_CONTEXT;
        close(fd);
        free(h);
        return NULL;
    }

    CUDA_EXTERNAL_MEMORY_HANDLE_DESC desc = {0};
    desc.type       = CU_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD;
    desc.handle.fd  = fd;
    desc.size       = size;
    desc.flags      = CUDA_EXTERNAL_MEMORY_DEDICATED;

    CUresult import_res = cuImportExternalMemory(&h->extMem, &desc);
    if (import_res != CUDA_SUCCESS) {
        if (out_import_err) *out_import_err = import_res;
        if (cu_ctx) cuCtxPopCurrent(&prev);
        close(fd);
        free(h);
        return NULL;
    }

    CUDA_EXTERNAL_MEMORY_BUFFER_DESC bufDesc = {0};
    bufDesc.offset = 0;
    bufDesc.size   = size;

    CUresult map_res = cuExternalMemoryGetMappedBuffer(&h->devPtr, h->extMem, &bufDesc);
    if (map_res != CUDA_SUCCESS) {
        if (out_map_err) *out_map_err = map_res;
        cuDestroyExternalMemory(h->extMem);
        if (cu_ctx) cuCtxPopCurrent(&prev);
        free(h);
        return NULL;
    }

    if (cu_ctx) cuCtxPopCurrent(&prev);
    return h;
}

static const char *cuda_result_name(CUresult res) {
    const char *name = NULL;
    if (cuGetErrorName(res, &name) != CUDA_SUCCESS || name == NULL) return "<unknown>";
    return name;
}

static const char *cuda_result_string(CUresult res) {
    const char *msg = NULL;
    if (cuGetErrorString(res, &msg) != CUDA_SUCCESS || msg == NULL) return "<unknown>";
    return msg;
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
static int nvenc_encode_devptr_diag_count = 0;

static int cloudplay_write_test_pattern_nv12(AVFrame *hw_frame, int width, int height) {
    size_t y_pitch = (size_t)width;
    size_t uv_pitch = (size_t)width;
    size_t y_size = y_pitch * (size_t)height;
    size_t uv_size = uv_pitch * (size_t)(height / 2);

    unsigned char *host_y = (unsigned char *)malloc(y_size);
    unsigned char *host_uv = (unsigned char *)malloc(uv_size);
    if (!host_y || !host_uv) {
        free(host_y);
        free(host_uv);
        return 0;
    }

    for (int y = 0; y < height; y++) {
        for (int x = 0; x < width; x++) {
            host_y[(size_t)y * y_pitch + (size_t)x] = (unsigned char)(16 + ((219 * x) / (width > 1 ? (width - 1) : 1)));
        }
    }
    memset(host_uv, 128, uv_size);

    CUDA_MEMCPY2D copy_y = {0};
    copy_y.srcMemoryType = CU_MEMORYTYPE_HOST;
    copy_y.srcHost       = host_y;
    copy_y.srcPitch      = y_pitch;
    copy_y.dstMemoryType = CU_MEMORYTYPE_DEVICE;
    copy_y.dstDevice     = (CUdeviceptr)(uintptr_t)hw_frame->data[0];
    copy_y.dstPitch      = (size_t)hw_frame->linesize[0];
    copy_y.WidthInBytes  = (size_t)width;
    copy_y.Height        = (size_t)height;

    CUDA_MEMCPY2D copy_uv = {0};
    copy_uv.srcMemoryType = CU_MEMORYTYPE_HOST;
    copy_uv.srcHost       = host_uv;
    copy_uv.srcPitch      = uv_pitch;
    copy_uv.dstMemoryType = CU_MEMORYTYPE_DEVICE;
    copy_uv.dstDevice     = (CUdeviceptr)(uintptr_t)hw_frame->data[1];
    copy_uv.dstPitch      = (size_t)hw_frame->linesize[1];
    copy_uv.WidthInBytes  = (size_t)width;
    copy_uv.Height        = (size_t)(height / 2);

    int ok = (cuMemcpy2D(&copy_y) == CUDA_SUCCESS && cuMemcpy2D(&copy_uv) == CUDA_SUCCESS);
    free(host_y);
    free(host_uv);
    return ok;
}

static uint8_t *nvenc_encode_devptr(nvenc_ctx *ctx, CUdeviceptr devPtr, size_t rgba_size, int *out_size) {
    *out_size = 0;
    int diag = (nvenc_encode_devptr_diag_count++ < 10) || (nvenc_encode_devptr_diag_count % 300 == 0);
    if (!devPtr || !ctx) {
        if (diag) fprintf(stderr, "[cloudplay diag] nvenc_encode_devptr: NULL devPtr=%p ctx=%p\n", (void*)devPtr, (void*)ctx);
        return NULL;
    }

    CUcontext prev = NULL;
    if (ctx->cu_ctx && cuCtxPushCurrent(ctx->cu_ctx) != CUDA_SUCCESS) {
        if (diag) fprintf(stderr, "[cloudplay diag] nvenc_encode_devptr: cuCtxPushCurrent failed\n");
        return NULL;
    }

    AVFrame *hw_frame = av_frame_alloc();
    if (!hw_frame) {
        if (diag) fprintf(stderr, "[cloudplay diag] nvenc_encode_devptr: av_frame_alloc failed\n");
        if (ctx->cu_ctx) cuCtxPopCurrent(&prev);
        return NULL;
    }

    int ret = av_hwframe_get_buffer(ctx->codec_ctx->hw_frames_ctx, hw_frame, 0);
    if (ret < 0) {
        if (diag) fprintf(stderr, "[cloudplay diag] nvenc_encode_devptr: av_hwframe_get_buffer failed ret=%d\n", ret);
        av_frame_free(&hw_frame);
        if (ctx->cu_ctx) cuCtxPopCurrent(&prev);
        return NULL;
    }
    if (diag) {
        fprintf(stderr,
                "[cloudplay diag] nvenc_encode_devptr: hw_frame layout w=%d h=%d linesizeY=%d linesizeUV=%d ptrY=%p ptrUV=%p neutralUV=%d\n",
                ctx->width, ctx->height,
                hw_frame->linesize[0], hw_frame->linesize[1],
                hw_frame->data[0], hw_frame->data[1],
                CLOUDPLAY_DIAG_FORCE_NEUTRAL_UV);
    }

    // Step 1: instrument the imported source buffer (devPtr) to confirm
    // whether it contains real RGBA pixels or is stale/empty.
    // We read 16 bytes from devPtr via cuMemcpyDtoH on the first 10 calls
    // and every 300 thereafter.
    if (diag) {
        unsigned char src_probe[16] = {0};
        CUresult probe_res = cuMemcpyDtoH(src_probe, devPtr, sizeof(src_probe));
        int nonzero = 0;
        for (int i = 0; i < 12; i += 4) {
            if (src_probe[i] || src_probe[i+1] || src_probe[i+2]) nonzero++;
        }
        fprintf(stderr,
                "[cloudplay diag] nvenc_encode_devptr: SOURCE PROBE devPtr=0x%llx probe_res=%d "
                "bytes=[%u,%u,%u,%u,%u,%u,%u,%u,%u,%u,%u,%u,%u,%u,%u,%u] nonzero_rgb_pixels=%d\n",
                (unsigned long long)devPtr, (int)probe_res,
                src_probe[0], src_probe[1], src_probe[2], src_probe[3],
                src_probe[4], src_probe[5], src_probe[6], src_probe[7],
                src_probe[8], src_probe[9], src_probe[10], src_probe[11],
                src_probe[12], src_probe[13], src_probe[14], src_probe[15],
                nonzero);
    }

    int color_ok = 0;
#if CLOUDPLAY_DIAG_DEST_PATTERN
    color_ok = cloudplay_write_test_pattern_nv12(hw_frame, ctx->width, ctx->height);
    if (diag) {
        unsigned char dst_y[16] = {0}, dst_uv[16] = {0};
        cuMemcpyDtoH(dst_y,  (CUdeviceptr)(uintptr_t)hw_frame->data[0], sizeof(dst_y));
        cuMemcpyDtoH(dst_uv, (CUdeviceptr)(uintptr_t)hw_frame->data[1], sizeof(dst_uv));
        fprintf(stderr,
                "[cloudplay diag] nvenc_encode_devptr: dest-pattern dstY=%u,%u,%u,%u dstUV=%u,%u,%u,%u\n",
                dst_y[0], dst_y[1], dst_y[2], dst_y[3],
                dst_uv[0], dst_uv[1], dst_uv[2], dst_uv[3]);
    }
#else
    // Attempt GPU colour conversion via PTX kernels.
    color_ok = cuda_rgba_to_nv12(devPtr, ctx->width, ctx->height);

    if (color_ok) {
        CUdeviceptr hw_y  = (CUdeviceptr)(uintptr_t)hw_frame->data[0];
        CUdeviceptr hw_uv = (CUdeviceptr)(uintptr_t)hw_frame->data[1];

        CUDA_MEMCPY2D copy_y = {0};
        copy_y.srcMemoryType = CU_MEMORYTYPE_DEVICE;
        copy_y.srcDevice     = g_color_ctx.nv12_y;
        copy_y.srcPitch      = (size_t)ctx->width;
        copy_y.dstMemoryType = CU_MEMORYTYPE_DEVICE;
        copy_y.dstDevice     = hw_y;
        copy_y.dstPitch      = (size_t)hw_frame->linesize[0];
        copy_y.WidthInBytes  = (size_t)ctx->width;
        copy_y.Height        = (size_t)ctx->height;

        CUDA_MEMCPY2D copy_uv = {0};
        copy_uv.srcMemoryType = CU_MEMORYTYPE_DEVICE;
        copy_uv.srcDevice     = g_color_ctx.nv12_uv;
        copy_uv.srcPitch      = (size_t)ctx->width;
        copy_uv.dstMemoryType = CU_MEMORYTYPE_DEVICE;
        copy_uv.dstDevice     = hw_uv;
        copy_uv.dstPitch      = (size_t)hw_frame->linesize[1];
        copy_uv.WidthInBytes  = (size_t)ctx->width;
        copy_uv.Height        = (size_t)(ctx->height / 2);

        if (cuMemcpy2D(&copy_y) != CUDA_SUCCESS ||
            cuMemcpy2D(&copy_uv) != CUDA_SUCCESS) {
            color_ok = 0;
        }
        if (color_ok && diag) {
            unsigned char src_y[16] = {0}, src_uv[16] = {0}, dst_y[16] = {0}, dst_uv[16] = {0};
            cuMemcpyDtoH(src_y,  g_color_ctx.nv12_y,  sizeof(src_y));
            cuMemcpyDtoH(src_uv, g_color_ctx.nv12_uv, sizeof(src_uv));
            cuMemcpyDtoH(dst_y,  hw_y,                sizeof(dst_y));
            cuMemcpyDtoH(dst_uv, hw_uv,               sizeof(dst_uv));
            fprintf(stderr,
                    "[cloudplay diag] nvenc_encode_devptr: samples srcY=%u,%u,%u,%u srcUV=%u,%u,%u,%u dstY=%u,%u,%u,%u dstUV=%u,%u,%u,%u\n",
                    src_y[0], src_y[1], src_y[2], src_y[3],
                    src_uv[0], src_uv[1], src_uv[2], src_uv[3],
                    dst_y[0], dst_y[1], dst_y[2], dst_y[3],
                    dst_uv[0], dst_uv[1], dst_uv[2], dst_uv[3]);
        }
    }
#endif

    if (!color_ok) {
        if (diag) fprintf(stderr, "[cloudplay diag] nvenc_encode_devptr: PTX color conversion failed, using Phase 3b raw fallback\n");
        // Phase 3b fallback: raw copy of RGBA bytes into Y plane only.
        // Colours are wrong but stream stays stable.
        size_t copy_size = rgba_size < (size_t)(ctx->width * ctx->height * 4)
                         ? rgba_size : (size_t)(ctx->width * ctx->height * 4);
        cuMemcpyDtoD((CUdeviceptr)(uintptr_t)hw_frame->data[0], devPtr, copy_size);
    } else {
#if CLOUDPLAY_DIAG_DEST_PATTERN
        if (diag) fprintf(stderr, "[cloudplay diag] nvenc_encode_devptr: destination test pattern OK\n");
#else
        if (diag) fprintf(stderr, "[cloudplay diag] nvenc_encode_devptr: PTX color conversion OK\n");
#endif
    }

    hw_frame->pts = ctx->pts++;
    ret = avcodec_send_frame(ctx->codec_ctx, hw_frame);
    av_frame_free(&hw_frame);
    if (ret < 0) {
        if (diag) fprintf(stderr, "[cloudplay diag] nvenc_encode_devptr: avcodec_send_frame failed ret=%d\n", ret);
        if (ctx->cu_ctx) cuCtxPopCurrent(&prev);
        return NULL;
    }

    av_packet_unref(ctx->packet);
    ret = avcodec_receive_packet(ctx->codec_ctx, ctx->packet);
    if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) {
        if (diag) fprintf(stderr, "[cloudplay diag] nvenc_encode_devptr: avcodec_receive_packet EAGAIN/EOF\n");
        if (ctx->cu_ctx) cuCtxPopCurrent(&prev);
        return NULL;
    }
    if (ret < 0) {
        if (diag) fprintf(stderr, "[cloudplay diag] nvenc_encode_devptr: avcodec_receive_packet error ret=%d\n", ret);
        if (ctx->cu_ctx) cuCtxPopCurrent(&prev);
        return NULL;
    }

    if (diag) fprintf(stderr, "[cloudplay diag] nvenc_encode_devptr: SUCCESS out_size=%d\n", ctx->packet->size);
    *out_size = ctx->packet->size;
    if (ctx->cu_ctx) cuCtxPopCurrent(&prev);
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
func ImportExternalMemory(enc *NVENC, fd int, size uint64) (uintptr, *ExtMemHandle, error) {
	extmemMu.Lock()
	defer extmemMu.Unlock()

	if enc == nil || enc.ctx == nil {
		return 0, nil, fmt.Errorf("nvenc: encoder/context not initialised")
	}

	var importErr C.CUresult
	var mapErr C.CUresult
	h := C.cuda_import_external_memory(enc.ctx.cu_ctx, C.int(fd), C.size_t(size), &importErr, &mapErr)
	if h == nil {
		if importErr != C.CUDA_SUCCESS {
			return 0, nil, fmt.Errorf("nvenc: cuda_import_external_memory failed for fd=%d size=%d (%s: %s)", fd, size, C.GoString(C.cuda_result_name(importErr)), C.GoString(C.cuda_result_string(importErr)))
		}
		if mapErr != C.CUDA_SUCCESS {
			return 0, nil, fmt.Errorf("nvenc: cuExternalMemoryGetMappedBuffer failed for fd=%d size=%d (%s: %s)", fd, size, C.GoString(C.cuda_result_name(mapErr)), C.GoString(C.cuda_result_string(mapErr)))
		}
		return 0, nil, fmt.Errorf("nvenc: external memory import failed for fd=%d size=%d (unknown CUDA error)", fd, size)
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

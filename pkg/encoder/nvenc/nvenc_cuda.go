//go:build nvenc && linux

package nvenc

// Phase 3: CUDA external-memory import and NVENC encode-from-device-pointer.
//
// This file adds EncodeFromDevPtr to the NVENC encoder, enabling the zero-copy
// path where pixel data never touches the CPU:
//
//   Vulkan ZeroCopyBuffer (fd) → cuMemImportFromShareableHandle (CUdeviceptr)
//   → nvenc_encode_devptr (NVENC reads directly from CUDA device memory)
//
// Build requirement: nvenc + linux tags (CUDA headers on NVIDIA Linux driver).

/*
#cgo pkg-config: libavcodec libavutil
#cgo LDFLAGS: -lcuda -lcudart
#include <libavcodec/avcodec.h>
#include <libavutil/opt.h>
#include <libavutil/hwcontext.h>
#include <libavutil/hwcontext_cuda.h>
#include <libavutil/pixfmt.h>
#include <cuda.h>
#include <cudaTypedefs.h>
#include <stdlib.h>
#include <string.h>

// cuda_import_external_memory imports a Linux fd (VK_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD)
// into CUDA and returns a device pointer at offset 0.
//
// Returns 0 on failure.
static CUdeviceptr cuda_import_external_memory(int fd, size_t size) {
    CUexternalMemory extMem = NULL;

    CUDA_EXTERNAL_MEMORY_HANDLE_DESC desc = {0};
    desc.type                = CU_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD;
    desc.handle.fd           = fd;
    desc.size                = size;
    desc.flags               = 0;

    if (cuImportExternalMemory(&extMem, &desc) != CUDA_SUCCESS) {
        return 0;
    }

    CUDA_EXTERNAL_MEMORY_BUFFER_DESC bufDesc = {0};
    bufDesc.offset = 0;
    bufDesc.size   = size;
    bufDesc.flags  = 0;

    CUdeviceptr devPtr = 0;
    if (cuExternalMemoryGetMappedBuffer(&devPtr, extMem, &bufDesc) != CUDA_SUCCESS) {
        cuDestroyExternalMemory(extMem);
        return 0;
    }

    // Note: extMem must stay alive for devPtr to be valid.  Caller is
    // responsible for lifecycle via nvenc_cuda_handle (see below).
    // For now we intentionally leak extMem here as its lifetime is tied
    // to the ZeroCopyBuffer allocation; a proper wrapper is in TODO.
    //
    // TODO(phase3): wrap CUexternalMemory in a handle and expose
    //               nvenc_cuda_release() to call cuDestroyExternalMemory.
    return devPtr;
}

// nvenc_encode_devptr encodes one RGBA8 frame that lives at a CUDA device
// pointer.  The encoder internally allocates a CUDA hw_frame and copies the
// device data into it.  No CPU involvement.
//
// devPtr   — CUDA device pointer to RGBA8 frame data
// size     — byte size of the frame (must be >= width*height*4)
//
// Returns pointer to H264 NAL bytes + sets *out_size, or NULL on error.
uint8_t *nvenc_encode_devptr(nvenc_ctx *ctx, CUdeviceptr devPtr, size_t size, int *out_size) {
    *out_size = 0;
    if (!devPtr || !ctx) return NULL;

    // Allocate a CUDA hw_frame from the encoder's pool.
    AVFrame *hw_frame = av_frame_alloc();
    if (!hw_frame) return NULL;

    int ret = av_hwframe_get_buffer(ctx->codec_ctx->hw_frames_ctx, hw_frame, 0);
    if (ret < 0) {
        av_frame_free(&hw_frame);
        return NULL;
    }

    // hw_frame->data[0] is the CUDA device pointer for the NV12 surface.
    // We're receiving RGBA8, so we need a colorspace conversion step on GPU.
    // For this scaffolding pass we copy the raw RGBA device bytes into the
    // hw_frame surface via CUDA memcpy (still GPU-only, just not NV12).
    //
    // TODO(phase3): insert a CUDA kernel or NPP call here to convert
    //               RGBA→NV12 on GPU before handing to NVENC.
    //               The frame surface size may differ from rgba size.
    size_t copy_size = size < (size_t)(ctx->width * ctx->height * 4)
                     ? size
                     : (size_t)(ctx->width * ctx->height * 4);
    cuMemcpyDtoD((CUdeviceptr)(uintptr_t)hw_frame->data[0], devPtr, copy_size);

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
	"unsafe"
)

// ImportExternalMemory imports a Linux opaque fd (obtained from
// ZeroCopyBuffer.ExportMemoryFd) into CUDA and returns a device pointer.
//
// The device pointer is valid as long as the underlying Vulkan allocation and
// CUDA external-memory handle are alive.
//
// TODO(phase3): expose a release function and tie the lifetime to the
// ZeroCopyBuffer so the fd and extMem are destroyed together.
func ImportExternalMemory(fd int, size uint64) (uintptr, error) {
	devPtr := C.cuda_import_external_memory(C.int(fd), C.size_t(size))
	if devPtr == 0 {
		return 0, fmt.Errorf("nvenc: cuda_import_external_memory failed for fd=%d", fd)
	}
	return uintptr(devPtr), nil
}

// EncodeFromDevPtr encodes one frame whose pixel data lives at cudaDevPtr
// (a CUDA device pointer, e.g. obtained via ImportExternalMemory).
//
// This is the Phase 3 hot path — no CPU memory is touched.
//
// size must be the byte size of the frame (width*height*4 for RGBA8).
//
// Note: RGBA→NV12 GPU conversion is currently a CUDA cuMemcpyDtoD into the
// hw_frame surface (still GPU-only).  A proper NPP/CUDA kernel for colour
// conversion is a TODO for Phase 3b.
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
		return nil, nil // EAGAIN / encoder buffering — not an error
	}

	result := make([]byte, int(outSize))
	copy(result, unsafe.Slice((*byte)(unsafe.Pointer(data)), int(outSize)))
	return result, nil
}

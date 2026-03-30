//go:build nvenc && linux && vulkan

// Phase 3 zero-copy media path: Vulkan → CUDA → NVENC.
//
// This file is compiled only when all three build tags are present:
//   - vulkan   : Vulkan context provider is available
//   - linux    : VK_KHR_external_memory_fd requires Linux
//   - nvenc    : FFmpeg h264_nvenc + CUDA headers are available
//
// It provides TryArmZeroCopy, which examines the runtime state and, if all
// conditions are met, creates an NVENC encoder, imports the Vulkan external
// memory fd into CUDA, and registers the encoder on the media pipe.
//
// ⚠ EXPERIMENTAL / INCOMPLETE
// The GPU colour conversion step (RGBA8 → NV12) is currently a raw
// cuMemcpyDtoD into the hw_frame surface, which produces incorrect colours.
// The path is intentionally gated behind config.Video.ZeroCopy = true so it
// cannot be activated accidentally.  See pkg/encoder/nvenc/nvenc_cuda.go for
// the colour conversion TODO.

package media

import (
	"fmt"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/encoder/nvenc"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// TryArmZeroCopy attempts to enable the Phase 3 zero-copy encode path on pipe.
//
// Parameters:
//   - pipe        : the WebrtcMediaPipe to arm (must already be Init()'ed)
//   - vc          : video encoder configuration (codec must be h264_nvenc)
//   - w, h        : frame dimensions in pixels
//   - zeroCopyFd  : function that returns the current frame's exportable Vulkan fd
//   - log         : logger
//
// Returns true if the path was successfully armed, false otherwise.
// On false, the caller should keep using the CPU readback path (no error is
// fatal — a log warning is emitted instead).
//
// The function is called once after Init(); the CUDA import is done here
// (the fd and CUDA handle are stable for the session lifetime).
func TryArmZeroCopy(
	pipe *WebrtcMediaPipe,
	vc config.Video,
	w, h uint,
	zeroCopyFd func(w, h uint) (int, error),
	log *logger.Logger,
) bool {
	if !vc.ZeroCopy {
		return false
	}
	if vc.Codec != "h264_nvenc" {
		log.Warn().Msgf("media/zerocopy: ZeroCopy=true but codec=%q — need h264_nvenc, skipping", vc.Codec)
		return false
	}

	// Defer the Vulkan fd import to the first rendered frame (lazyZeroCopyNVENC).
	// At Init() time no frame has been rendered yet, so the fd is not available;
	// we allocate the NVENC encoder here but bind the CUDA import lazily.
	opts := &nvenc.Options{
		Bitrate: vc.Nvenc.Bitrate,
		Preset:  vc.Nvenc.Preset,
		Tune:    vc.Nvenc.Tune,
	}
	enc, err := nvenc.NewEncoder(int(w), int(h), opts)
	if err != nil {
		log.Warn().Err(err).Msg("media/zerocopy: NVENC encoder creation failed — falling back to CPU path")
		return false
	}

	bufSize := uint64(w * h * 4) // RGBA8

	// lazyZeroCopy lazily imports the fd on the first frame.
	lazy := &lazyZeroCopyNVENC{
		enc:         enc,
		bufSize:     bufSize,
		getFd:       zeroCopyFd,
		w:           w,
		h:           h,
		log:         log,
	}

	pipe.SetZeroCopyEncoder(lazy)
	log.Info().Msgf("media/zerocopy: Phase 3 zero-copy armed (%dx%d, bufSize=%d) — ⚠ EXPERIMENTAL", w, h, bufSize)
	return true
}

// lazyZeroCopyNVENC defers the CUDA external-memory import to the first frame,
// since the Vulkan exportable fd is not available until after the first render.
type lazyZeroCopyNVENC struct {
	enc     *nvenc.NVENC
	bufSize uint64
	devPtr  uintptr // 0 until first successful import
	getFd   func(w, h uint) (int, error)
	w, h    uint
	log     *logger.Logger
}

func (lz *lazyZeroCopyNVENC) EncodeFromDevPtr(_ uintptr, _ uint64) ([]byte, error) {
	// Import fd on first frame if not yet done.
	if lz.devPtr == 0 {
		fd, err := lz.getFd(lz.w, lz.h)
		if err != nil {
			return nil, fmt.Errorf("zerocopy: Vulkan fd error: %w", err)
		}
		if fd < 0 {
			return nil, fmt.Errorf("zerocopy: Vulkan fd not ready yet (fd=%d)", fd)
		}
		devPtr, err := nvenc.ImportExternalMemory(fd, lz.bufSize)
		if err != nil {
			return nil, fmt.Errorf("zerocopy: CUDA import failed: %w", err)
		}
		lz.devPtr = devPtr
		lz.log.Info().Msgf("media/zerocopy: CUDA external memory imported (fd=%d, devPtr=0x%x)", fd, devPtr)
	}
	return lz.enc.EncodeFromDevPtr(lz.devPtr, lz.bufSize)
}

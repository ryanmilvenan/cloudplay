//go:build nvenc && linux && vulkan

// Phase 3c zero-copy media path: Vulkan → CUDA → NVENC with GPU colour conversion.
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
// Colour conversion (Phase 3c)
// ─────────────────────────────
// The GPU RGBA→NV12 conversion is performed by an embedded PTX kernel loaded
// at runtime via cuModuleLoadData/cuLaunchKernel.  This uses BT.601 studio-
// swing coefficients (matching the CPU libyuv path) and runs entirely on the
// GPU — no CPU involvement at all in the encode hot path.
//
// If PTX JIT fails (very old driver), the path falls back transparently to
// the Phase 3b raw-copy behaviour (wrong colours, but stable stream).  The
// CPU encode path is never affected.
//
// The path is gated behind config.Video.ZeroCopy = true (default: false).

package media

import (
	"fmt"
	"log"
	"sync/atomic"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/encoder/nvenc"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

var zcDiagFrame int64

// TryArmZeroCopy attempts to enable the Phase 3c zero-copy encode path on pipe.
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
// CUDA external-memory import is deferred to the first rendered frame via
// lazyZeroCopyNVENC, since no frame fd is available at Init() time.
func TryArmZeroCopy(
	pipe *WebrtcMediaPipe,
	vc config.Video,
	w, h uint,
	zeroCopyFd func(w, h uint) (int, uint64, error),
	waitBlit func() error,
	log *logger.Logger,
) bool {
	if !vc.ZeroCopy {
		return false
	}
	if vc.Codec != "h264_nvenc" {
		log.Warn().Msgf("media/zerocopy: ZeroCopy=true but codec=%q — need h264_nvenc, skipping", vc.Codec)
		return false
	}

	opts := &nvenc.Options{
		Bitrate:          vc.Nvenc.Bitrate,
		Preset:           vc.Nvenc.Preset,
		Tune:             vc.Nvenc.Tune,
		Profile:          vc.Nvenc.Profile,
		KeyframeInterval: vc.Nvenc.KeyframeInterval,
		ZeroCopy:         true,
	}
	enc, err := nvenc.NewEncoder(int(w), int(h), opts)
	if err != nil {
		log.Warn().Err(err).Msg("media/zerocopy: NVENC encoder creation failed — falling back to CPU path")
		return false
	}

	frameSize := uint64(w * h * 4) // visible RGBA8 bytes

	lazy := &lazyZeroCopyNVENC{
		enc:       enc,
		frameSize: frameSize,
		getFd:     zeroCopyFd,
		waitBlit:  waitBlit,
		w:         w,
		h:         h,
		log:       log,
	}

	pipe.SetZeroCopyEncoder(lazy)
	log.Info().Msgf("media/zerocopy: Phase 3c zero-copy armed (%dx%d, bufSize=%d) — GPU RGBA→NV12 via PTX kernel", w, h, frameSize)
	return true
}

// lazyZeroCopyNVENC defers the CUDA external-memory import to the first frame,
// since the Vulkan exportable fd is not available until after the first render.
//
// On cleanup, ReleaseOnDestroy must be called to free the CUDA handle.
type lazyZeroCopyNVENC struct {
	enc        *nvenc.NVENC
	frameSize  uint64
	importSize uint64
	devPtr     uintptr // 0 until first successful import
	extMem     *nvenc.ExtMemHandle // non-nil after first successful CUDA import
	getFd      func(w, h uint) (int, uint64, error)
	waitBlit   func() error // waits for async BlitFrom to complete; may be nil
	w, h       uint
	log        *logger.Logger
}

// EncodeFromDevPtr is the hot path: called every frame for zero-copy encode.
//
// On the first call it imports the Vulkan fd into CUDA (lazy, since the fd is
// not available until after the first rendered frame).  Subsequent calls reuse
// the cached devPtr.
//
// The actual CUDA device pointer points to the Vulkan-exported RGBA8 buffer.
// The NVENC EncodeFromDevPtr method runs the PTX RGBA→NV12 kernel on GPU before
// handing the converted surface to NVENC.
func (lz *lazyZeroCopyNVENC) EncodeFromDevPtr(_ uintptr, _ uint64) ([]byte, error) {
	// Import fd on first frame if not yet done.
	if lz.devPtr == 0 {
		fd, importSize, err := lz.getFd(lz.w, lz.h)
		if err != nil {
			return nil, fmt.Errorf("zerocopy: Vulkan fd error: %w", err)
		}
		if fd < 0 {
			return nil, fmt.Errorf("zerocopy: Vulkan fd not ready yet (fd=%d)", fd)
		}
		if importSize == 0 {
			return nil, fmt.Errorf("zerocopy: Vulkan export size invalid (fd=%d, size=%d)", fd, importSize)
		}
		log.Printf("[cloudplay diag] zerocopy: attempting CUDA import fd=%d importSize=%d", fd, importSize)
		devPtr, extMem, err := nvenc.ImportExternalMemory(lz.enc, fd, importSize)
		if err != nil {
			return nil, fmt.Errorf("zerocopy: CUDA import failed: %w", err)
		}
		lz.devPtr = devPtr
		lz.extMem = extMem
		lz.importSize = importSize
		log.Printf("[cloudplay diag] zerocopy: CUDA import SUCCESS devPtr=0x%x importSize=%d frameSize=%d", devPtr, importSize, lz.frameSize)
		lz.log.Info().Msgf("media/zerocopy: CUDA external memory imported (fd=%d, importSize=%d, devPtr=0x%x)", fd, importSize, devPtr)
	}
	// Wait for the async blit to complete before reading the GPU buffer.
	if lz.waitBlit != nil {
		if err := lz.waitBlit(); err != nil {
			return nil, fmt.Errorf("zerocopy: WaitBlit failed: %w", err)
		}
	}
	out, err := lz.enc.EncodeFromDevPtr(lz.devPtr, lz.frameSize)
	if err == nil && len(out) == 0 {
		n := atomic.AddInt64(&zcDiagFrame, 1)
		if n <= 5 || n%300 == 0 {
			log.Printf("[cloudplay diag] zerocopy: EncodeFromDevPtr returned nil output (no error), devPtr=0x%x frameSize=%d call=%d", lz.devPtr, lz.frameSize, n)
		}
	}
	return out, err
}

// Destroy releases the CUDA external-memory handle and shuts down the encoder.
// Must be called when the media pipe is torn down.
func (lz *lazyZeroCopyNVENC) Destroy() {
	if lz.extMem != nil {
		nvenc.ReleaseExternalMemory(lz.extMem)
		lz.extMem = nil
	}
	if lz.enc != nil {
		lz.enc.Shutdown()
		lz.enc = nil
	}
}

package media

import (
	"fmt"
	"sync"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/encoder"
	"github.com/giongto35/cloud-game/v3/pkg/encoder/opus"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
)

// ZeroCopyVideoEncoder is the interface for Phase 3 GPU-only encode.
//
// Implementations (e.g. NVENC) receive a CUDA device pointer directly from
// the Vulkan external-memory import and encode without touching the CPU.
//
// EncodeFromDevPtr takes a CUDA device pointer and the byte size of the
// frame buffer (width*height*4 for RGBA8).  It returns the encoded H264
// NAL bytes, or nil if the encoder is still buffering (EAGAIN).
//
// ⚠ EXPERIMENTAL: GPU RGBA→NV12 colour conversion is not yet a proper
// CUDA/NPP kernel; the current path produces incorrect colours.
// See pkg/encoder/nvenc/nvenc_cuda.go TODO for details.
type ZeroCopyVideoEncoder interface {
	EncodeFromDevPtr(cudaDevPtr uintptr, size uint64) ([]byte, error)
}

const audioHz = 48000

type samples []int16

var (
	encoderOnce = sync.Once{}
	opusCoder   *opus.Encoder
)

func DefaultOpus() (*opus.Encoder, error) {
	var err error
	encoderOnce.Do(func() { opusCoder, err = opus.NewEncoder(audioHz) })
	if err != nil {
		return nil, err
	}
	if err = opusCoder.Reset(); err != nil {
		return nil, err
	}
	return opusCoder, nil
}

type WebrtcMediaPipe struct {
	a        *opus.Encoder
	v        *encoder.Video
	onAudio  func([]byte, float32)
	audioBuf *buffer
	log      *logger.Logger

	mua sync.RWMutex
	muv sync.RWMutex

	aConf config.Audio
	vConf config.Video

	AudioSrcHz     int
	AudioFrames    []float32
	VideoW, VideoH int
	VideoScale     float64

	initialized bool

	// keep the old settings for reinit
	oldPf   uint32
	oldRot  uint
	oldFlip bool

	// Phase 3 zero-copy path.
	// zcEnc is non-nil only when zeroCopyEnabled is true AND NVENC+Vulkan are
	// both available at runtime.  When non-nil, ProcessVideoDevPtr drives
	// encoding directly from a CUDA device pointer without CPU involvement.
	//
	// ⚠ EXPERIMENTAL: colour conversion is incomplete — see ZeroCopyVideoEncoder.
	zcMu            sync.RWMutex
	zcEnc           ZeroCopyVideoEncoder
	zeroCopyEnabled bool // mirrors config.Video.ZeroCopy
}

func NewWebRtcMediaPipe(ac config.Audio, vc config.Video, log *logger.Logger) *WebrtcMediaPipe {
	return &WebrtcMediaPipe{log: log, aConf: ac, vConf: vc}
}

func (wmp *WebrtcMediaPipe) SetAudioCb(cb func([]byte, int32)) {
	wmp.onAudio = func(bytes []byte, ms float32) {
		cb(bytes, int32(time.Duration(ms)*time.Millisecond))
	}
}
func (wmp *WebrtcMediaPipe) Destroy() {
	v := wmp.Video()
	if v != nil {
		v.Stop()
	}
}
func (wmp *WebrtcMediaPipe) PushAudio(audio []int16) {
	wmp.audioBuf.write(audio, wmp.encodeAudio)
}

func (wmp *WebrtcMediaPipe) Init() error {
	if err := wmp.initAudio(wmp.AudioSrcHz, wmp.AudioFrames); err != nil {
		return err
	}
	if err := wmp.initVideo(wmp.VideoW, wmp.VideoH, wmp.VideoScale, wmp.vConf); err != nil {
		return err
	}

	a := wmp.Audio()
	v := wmp.Video()

	if v == nil || a == nil {
		return fmt.Errorf("could intit the encoders, v=%v a=%v", v != nil, a != nil)
	}

	wmp.log.Debug().Msgf("%v", v.Info())
	wmp.initialized = true
	return nil
}

func (wmp *WebrtcMediaPipe) initAudio(srcHz int, frameSizes []float32) error {
	au, err := DefaultOpus()
	if err != nil {
		return fmt.Errorf("opus fail: %w", err)
	}
	wmp.log.Debug().Msgf("Opus: %v", au.GetInfo())
	wmp.SetAudio(au)
	buf, err := newBuffer(frameSizes, srcHz)
	if err != nil {
		return err
	}
	wmp.log.Debug().Msgf("Opus frames (ms): %v", frameSizes)
	dstHz, _ := au.SampleRate()
	if srcHz != dstHz {
		buf.resample(dstHz, ResampleAlgo(wmp.aConf.Resampler))
		wmp.log.Debug().Msgf("Resample %vHz -> %vHz", srcHz, dstHz)
	}
	wmp.audioBuf = buf
	return nil
}

func (wmp *WebrtcMediaPipe) encodeAudio(pcm samples, ms float32) {
	data, err := wmp.Audio().Encode(pcm)
	if err != nil {
		wmp.log.Error().Err(err).Msgf("opus encode fail")
		return
	}
	wmp.onAudio(data, ms)
}

func (wmp *WebrtcMediaPipe) initVideo(w, h int, scale float64, conf config.Video) (err error) {
	sw, sh := round(w, scale), round(h, scale)
	enc, err := encoder.NewVideoEncoder(w, h, sw, sh, scale, conf, wmp.log)
	if err != nil {
		return err
	}
	if enc == nil {
		return fmt.Errorf("broken video encoder init")
	}
	wmp.SetVideo(enc)
	wmp.log.Debug().Msgf("media scale: %vx%v -> %vx%v", w, h, sw, sh)
	return err
}

func round(x int, scale float64) int { return (int(float64(x)*scale) + 1) & ^1 }

// ProcessVideo encodes one video frame and returns H264 NAL bytes.
//
// Routing:
//   - If the Phase 3 zero-copy path is armed (ZeroCopyActive == true), it
//     attempts GPU-direct encoding via ProcessVideoZeroCopy first.
//   - On success (non-nil result) the zero-copy bytes are returned.
//   - On failure or nil (EAGAIN / fd not yet ready), falls through to the
//     standard CPU readback path.
//
// The CPU fallback ensures backward compatibility with non-Vulkan/non-NVENC
// builds and guarantees continued operation when zero-copy is gated off.
func (wmp *WebrtcMediaPipe) ProcessVideo(v app.Video) []byte {
	// Phase 3 fast path: attempt zero-copy GPU encode.
	if wmp.ZeroCopyActive() {
		if out := wmp.ProcessVideoZeroCopy(); out != nil {
			return out
		}
		// Zero-copy returned nil (first frame before fd is ready, EAGAIN, or
		// encode error).  Fall through to CPU path silently.
	}

	// CPU path: standard YUV conversion + software / NVENC CPU-upload encode.
	return wmp.Video().Encode(encoder.InFrame(v.Frame))
}

// SetZeroCopyEncoder registers the NVENC encoder for the Phase 3 path and
// marks zero-copy as active for this pipe.  Pass nil to disable.
//
// Must be called before the first ProcessVideoDevPtr invocation.
// Thread-safe.
func (wmp *WebrtcMediaPipe) SetZeroCopyEncoder(enc ZeroCopyVideoEncoder) {
	wmp.zcMu.Lock()
	wmp.zcEnc = enc
	wmp.zeroCopyEnabled = enc != nil && wmp.vConf.ZeroCopy
	wmp.zcMu.Unlock()
	if wmp.zeroCopyEnabled {
		wmp.log.Info().Msg("media: Phase 3 zero-copy NVENC path armed (⚠ experimental — colours may be wrong)")
	}
}

// ZeroCopyActive reports whether the Phase 3 zero-copy path is armed and
// ready for use.  Returns false if the config flag is off or no encoder is set.
func (wmp *WebrtcMediaPipe) ZeroCopyActive() bool {
	wmp.zcMu.RLock()
	ok := wmp.zeroCopyEnabled && wmp.zcEnc != nil
	wmp.zcMu.RUnlock()
	return ok
}

// ProcessVideoZeroCopy encodes one frame via the Phase 3 zero-copy path.
//
// The actual CUDA device pointer is managed internally by the ZeroCopyVideoEncoder
// implementation (see lazyZeroCopyNVENC in zerocopy.go).  The encoder imports
// the Vulkan external-memory fd on first call and caches the devPtr for the
// session lifetime — the caller does not need to manage memory handles.
//
// Returns nil if the encoder is still buffering (EAGAIN), if the fd is not
// yet available (before the first frame), or if the zero-copy path is not
// armed.  Callers must fall back to ProcessVideo on nil.
//
// ⚠ EXPERIMENTAL: GPU RGBA→NV12 colour conversion is not yet correct.
// See pkg/encoder/nvenc/nvenc_cuda.go for the TODO.
func (wmp *WebrtcMediaPipe) ProcessVideoZeroCopy() []byte {
	wmp.zcMu.RLock()
	enc := wmp.zcEnc
	ok := wmp.zeroCopyEnabled
	wmp.zcMu.RUnlock()

	if !ok || enc == nil {
		return nil
	}

	// Pass (0, 0) — the lazyZeroCopyNVENC implementation ignores these args
	// and manages devPtr + bufSize internally via its getFd closure.
	out, err := enc.EncodeFromDevPtr(0, 0)
	if err != nil {
		wmp.log.Error().Err(err).Msg("media: zero-copy encode error")
		return nil
	}
	return out
}

func (wmp *WebrtcMediaPipe) Reinit() error {
	if !wmp.initialized {
		return nil
	}

	wmp.Video().Stop()
	if err := wmp.initVideo(wmp.VideoW, wmp.VideoH, wmp.VideoScale, wmp.vConf); err != nil {
		return err
	}
	// restore old
	wmp.SetPixFmt(wmp.oldPf)
	wmp.SetRot(wmp.oldRot)
	wmp.SetVideoFlip(wmp.oldFlip)
	return nil
}

func (wmp *WebrtcMediaPipe) IsInitialized() bool { return wmp.initialized }
func (wmp *WebrtcMediaPipe) SetPixFmt(f uint32)  { wmp.oldPf = f; wmp.v.SetPixFormat(f) }
func (wmp *WebrtcMediaPipe) SetVideoFlip(b bool) { wmp.oldFlip = b; wmp.v.SetFlip(b) }
func (wmp *WebrtcMediaPipe) SetRot(r uint)       { wmp.oldRot = r; wmp.v.SetRot(r) }

func (wmp *WebrtcMediaPipe) Video() *encoder.Video {
	wmp.muv.RLock()
	defer wmp.muv.RUnlock()
	return wmp.v
}

func (wmp *WebrtcMediaPipe) SetVideo(e *encoder.Video) {
	wmp.muv.Lock()
	wmp.v = e
	wmp.muv.Unlock()
}

func (wmp *WebrtcMediaPipe) Audio() *opus.Encoder {
	wmp.mua.RLock()
	defer wmp.mua.RUnlock()
	return wmp.a
}

func (wmp *WebrtcMediaPipe) SetAudio(e *opus.Encoder) {
	wmp.mua.Lock()
	wmp.a = e
	wmp.mua.Unlock()
}

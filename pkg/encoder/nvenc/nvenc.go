//go:build nvenc

package nvenc

/*
#cgo pkg-config: libavcodec libavutil
#cgo LDFLAGS: -lcuda
#include "nvenc_ctx.h"
#include <libavutil/opt.h>
#include <libavutil/hwcontext.h>
#include <libavutil/hwcontext_cuda.h>
#include <libavutil/pixfmt.h>
#include <stdlib.h>
#include <string.h>

// nvenc_new allocates and opens an h264_nvenc encoder.
// Returns NULL and sets last_err on failure.
static const char *last_err = "";
nvenc_ctx *nvenc_new(int width, int height, int bitrate_kbps, const char *preset, const char *tune, const char *profile, int keyframe_interval, int zero_copy) {
    int ret;

    const AVCodec *codec = avcodec_find_encoder_by_name("h264_nvenc");
    if (!codec) {
        last_err = "h264_nvenc encoder not found (FFmpeg not built with NVENC support)";
        return NULL;
    }

    nvenc_ctx *ctx = (nvenc_ctx *)calloc(1, sizeof(nvenc_ctx));
    if (!ctx) {
        last_err = "out of memory";
        return NULL;
    }
    ctx->width   = width;
    ctx->height  = height;
    ctx->y_size  = width * height;
    ctx->uv_size = ctx->y_size >> 2;

    // Create CUDA hardware device context
    ret = av_hwdevice_ctx_create(&ctx->hw_device_ctx, AV_HWDEVICE_TYPE_CUDA, NULL, NULL, 0);
    if (ret < 0) {
        last_err = "failed to create CUDA hardware device context";
        free(ctx);
        return NULL;
    }

    // Extract the CUcontext from FFmpeg's hw device context for use by nvenc_cuda.go
    {
        AVHWDeviceContext *dev_ctx = (AVHWDeviceContext *)ctx->hw_device_ctx->data;
        AVCUDADeviceContext *cuda_ctx = (AVCUDADeviceContext *)dev_ctx->hwctx;
        ctx->cu_ctx = cuda_ctx->cuda_ctx;
    }

    ctx->codec_ctx = avcodec_alloc_context3(codec);
    if (!ctx->codec_ctx) {
        last_err = "failed to allocate codec context";
        av_buffer_unref(&ctx->hw_device_ctx);
        free(ctx);
        return NULL;
    }

    ctx->codec_ctx->width     = width;
    ctx->codec_ctx->height    = height;
    ctx->codec_ctx->time_base = (AVRational){1, 60};
    ctx->codec_ctx->framerate = (AVRational){60, 1};
    ctx->codec_ctx->pix_fmt   = AV_PIX_FMT_CUDA;  // hardware frames
    ctx->codec_ctx->hw_device_ctx = av_buffer_ref(ctx->hw_device_ctx);
    ctx->codec_ctx->max_b_frames = 0;
    ctx->codec_ctx->flags |= AV_CODEC_FLAG_LOW_DELAY;

    // Rate control: CBR at given bitrate
    ctx->codec_ctx->bit_rate     = (int64_t)bitrate_kbps * 1000;
    ctx->codec_ctx->rc_max_rate  = (int64_t)bitrate_kbps * 1000;
    // Tight rc buffer (~2 frames at 60fps) to avoid burst/starvation cycles
    ctx->codec_ctx->rc_buffer_size = (int64_t)bitrate_kbps * 1000 / 30;

    // Low-latency options via AVOptions
    if (preset && preset[0]) {
        av_opt_set(ctx->codec_ctx->priv_data, "preset", preset, 0);
    } else {
        av_opt_set(ctx->codec_ctx->priv_data, "preset", "p4", 0);  // balanced low-latency
    }
    if (tune && tune[0]) {
        av_opt_set(ctx->codec_ctx->priv_data, "tune", tune, 0);
    } else {
        av_opt_set(ctx->codec_ctx->priv_data, "tune", "ll", 0);  // low latency
    }
    av_opt_set(ctx->codec_ctx->priv_data, "rc", "cbr", 0);
    av_opt_set(ctx->codec_ctx->priv_data, "zerolatency", "1", 0);
    av_opt_set_int(ctx->codec_ctx->priv_data, "delay", 0, 0);
    av_opt_set(ctx->codec_ctx->priv_data, "rc-lookahead", "0", 0);
    if (profile && profile[0]) {
        av_opt_set(ctx->codec_ctx->priv_data, "profile", profile, 0);
    } else {
        av_opt_set(ctx->codec_ctx->priv_data, "profile", "baseline", 0);
    }
    if (keyframe_interval > 0) {
        ctx->codec_ctx->gop_size = keyframe_interval;
    } else {
        ctx->codec_ctx->gop_size = 120; // default: IDR every 2s at 60fps
    }

    // Set up hardware frames context so the encoder gets CUDA surfaces
    AVBufferRef *hw_frames_ref = av_hwframe_ctx_alloc(ctx->hw_device_ctx);
    if (!hw_frames_ref) {
        last_err = "failed to allocate hw frames context";
        avcodec_free_context(&ctx->codec_ctx);
        av_buffer_unref(&ctx->hw_device_ctx);
        free(ctx);
        return NULL;
    }
    AVHWFramesContext *frames_ctx = (AVHWFramesContext *)hw_frames_ref->data;
    frames_ctx->format    = AV_PIX_FMT_CUDA;
    frames_ctx->sw_format = zero_copy ? AV_PIX_FMT_NV12 : AV_PIX_FMT_YUV420P;
    frames_ctx->width     = width;
    frames_ctx->height    = height;
    frames_ctx->initial_pool_size = 8;
    ret = av_hwframe_ctx_init(hw_frames_ref);
    if (ret < 0) {
        last_err = "failed to init hw frames context";
        av_buffer_unref(&hw_frames_ref);
        avcodec_free_context(&ctx->codec_ctx);
        av_buffer_unref(&ctx->hw_device_ctx);
        free(ctx);
        return NULL;
    }
    ctx->codec_ctx->hw_frames_ctx = hw_frames_ref;  // takes ownership

    ret = avcodec_open2(ctx->codec_ctx, codec, NULL);
    if (ret < 0) {
        last_err = "failed to open h264_nvenc codec";
        avcodec_free_context(&ctx->codec_ctx);
        av_buffer_unref(&ctx->hw_device_ctx);
        free(ctx);
        return NULL;
    }

    // Allocate software frame for CPU→GPU upload
    ctx->frame = av_frame_alloc();
    if (!ctx->frame) {
        last_err = "failed to allocate AVFrame";
        avcodec_free_context(&ctx->codec_ctx);
        av_buffer_unref(&ctx->hw_device_ctx);
        free(ctx);
        return NULL;
    }
    ctx->frame->format = AV_PIX_FMT_YUV420P;
    ctx->frame->width  = width;
    ctx->frame->height = height;

    ctx->packet = av_packet_alloc();
    if (!ctx->packet) {
        last_err = "failed to allocate AVPacket";
        av_frame_free(&ctx->frame);
        avcodec_free_context(&ctx->codec_ctx);
        av_buffer_unref(&ctx->hw_device_ctx);
        free(ctx);
        return NULL;
    }

    last_err = "";
    return ctx;
}

// nvenc_encode encodes one I420 frame.
// yuv must be width*height*3/2 bytes: [Y plane][U plane][V plane].
// Returns pointer to encoded H264 NAL data and sets *out_size.
// The returned buffer is owned by ctx->packet; valid until next encode call.
uint8_t *nvenc_encode(nvenc_ctx *ctx, uint8_t *yuv, int *out_size) {
    int ret;
    *out_size = 0;

    // Set up sw_frame planes pointing into the caller's yuv buffer
    ctx->frame->data[0] = yuv;                                  // Y
    ctx->frame->data[1] = yuv + ctx->y_size;                    // U
    ctx->frame->data[2] = yuv + ctx->y_size + ctx->uv_size;     // V
    ctx->frame->linesize[0] = ctx->width;
    ctx->frame->linesize[1] = ctx->width >> 1;
    ctx->frame->linesize[2] = ctx->width >> 1;
    ctx->frame->pts = ctx->pts++;

    // Allocate a CUDA hw_frame and upload the sw_frame into it
    AVFrame *hw_frame = av_frame_alloc();
    if (!hw_frame) {
        return NULL;
    }
    ret = av_hwframe_get_buffer(ctx->codec_ctx->hw_frames_ctx, hw_frame, 0);
    if (ret < 0) {
        av_frame_free(&hw_frame);
        return NULL;
    }
    ret = av_hwframe_transfer_data(hw_frame, ctx->frame, 0);
    if (ret < 0) {
        av_frame_free(&hw_frame);
        return NULL;
    }
    hw_frame->pts = ctx->frame->pts;

    // Send the hw frame to the encoder
    ret = avcodec_send_frame(ctx->codec_ctx, hw_frame);
    av_frame_free(&hw_frame);
    if (ret < 0) {
        return NULL;
    }

    // Receive encoded packet
    av_packet_unref(ctx->packet);
    ret = avcodec_receive_packet(ctx->codec_ctx, ctx->packet);
    if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) {
        return NULL;
    }
    if (ret < 0) {
        return NULL;
    }

    *out_size = ctx->packet->size;
    return ctx->packet->data;
}

// nvenc_intra_refresh requests the next frame to be an IDR.
void nvenc_intra_refresh(nvenc_ctx *ctx) {
    if (!ctx || !ctx->codec_ctx) return;
    // Force the next frame as an intra frame via AV_FRAME_FLAG_KEY on next encode;
    // with zerolatency CBR NVENC, sending a NULL frame won't flush mid-stream,
    // so instead we set the force_key_frame option on the private context.
    av_opt_set_int(ctx->codec_ctx->priv_data, "forced-idr", 1, 0);
}

// nvenc_destroy frees all resources.
void nvenc_destroy(nvenc_ctx *ctx) {
    if (!ctx) return;
    if (ctx->packet)    av_packet_free(&ctx->packet);
    if (ctx->frame)     av_frame_free(&ctx->frame);
    if (ctx->codec_ctx) avcodec_free_context(&ctx->codec_ctx);
    if (ctx->hw_device_ctx) av_buffer_unref(&ctx->hw_device_ctx);
    free(ctx);
}

const char *nvenc_last_error() {
    return last_err;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// NVENC implements the encoder.Encoder interface using FFmpeg's h264_nvenc.
// Build with the `nvenc` build tag to include this encoder.
type NVENC struct {
	ctx              *C.nvenc_ctx
	width            int
	height           int
	preset           string
	tune             string
	profile          string
	bitrate          int
	keyframeInterval int
	flipped          bool
}

// Options configures the NVENC encoder.
type Options struct {
	// Bitrate in kbps for CBR mode (default: 4000)
	Bitrate int
	// NVENC preset: p1 (fastest) … p7 (best quality). Default: "p4"
	Preset string
	// NVENC tune: ll (low latency), ull (ultra low latency), hq (high quality). Default: "ll"
	Tune string
	// H.264 profile: baseline, main, high. Default: "high"
	Profile string
	// Keyframe interval in frames. 0 = encoder default. Default: 120 (2s at 60fps)
	KeyframeInterval int
	// ZeroCopy enables the Phase 3c GPU-direct encode path.
	ZeroCopy bool
}

// NewEncoder creates a new NVENC encoder for the given dimensions.
func NewEncoder(w, h int, opts *Options) (*NVENC, error) {
	if opts == nil {
		opts = &Options{
			Bitrate: 4000,
			Preset:  "p4",
			Tune:    "ll",
		}
	}
	if opts.Bitrate <= 0 {
		opts.Bitrate = 4000
	}
	if opts.Preset == "" {
		opts.Preset = "p4"
	}
	if opts.Tune == "" {
		opts.Tune = "ll"
	}
	if opts.Profile == "" {
		opts.Profile = "baseline"
	}
	if opts.KeyframeInterval <= 0 {
		opts.KeyframeInterval = 120 // 2s at 60fps
	}

	preset := C.CString(opts.Preset)
	tune := C.CString(opts.Tune)
	profile := C.CString(opts.Profile)
	defer C.free(unsafe.Pointer(preset))
	defer C.free(unsafe.Pointer(tune))
	defer C.free(unsafe.Pointer(profile))

	zcFlag := C.int(0)
	if opts.ZeroCopy {
		zcFlag = C.int(1)
	}
	ctx := C.nvenc_new(C.int(w), C.int(h), C.int(opts.Bitrate), preset, tune, profile, C.int(opts.KeyframeInterval), zcFlag)
	if ctx == nil {
		errMsg := C.GoString(C.nvenc_last_error())
		return nil, fmt.Errorf("nvenc: %s", errMsg)
	}

	return &NVENC{
		ctx:              ctx,
		width:            w,
		height:           h,
		preset:           opts.Preset,
		tune:             opts.Tune,
		profile:          opts.Profile,
		bitrate:          opts.Bitrate,
		keyframeInterval: opts.KeyframeInterval,
	}, nil
}

// Encode encodes a single YUV I420 frame and returns H264 NAL units.
// The input slice must be width*height*3/2 bytes: [Y][U][V].
func (e *NVENC) Encode(yuv []byte) []byte {
	if len(yuv) == 0 {
		return nil
	}

	var outSize C.int
	data := C.nvenc_encode(e.ctx, (*C.uint8_t)(unsafe.SliceData(yuv)), &outSize)
	if data == nil || outSize == 0 {
		return nil
	}
	// Copy out before the next encode call overwrites the packet buffer.
	result := make([]byte, int(outSize))
	copy(result, unsafe.Slice((*byte)(unsafe.Pointer(data)), int(outSize)))
	return result
}

// IntraRefresh requests that the next encoded frame be an IDR frame.
func (e *NVENC) IntraRefresh() {
	if e.ctx != nil {
		C.nvenc_intra_refresh(e.ctx)
	}
}

// Info returns a human-readable description of the encoder.
func (e *NVENC) Info() string {
	return fmt.Sprintf("h264_nvenc (preset=%s, tune=%s, profile=%s, bitrate=%dkbps, gop=%d)", e.preset, e.tune, e.profile, e.bitrate, e.keyframeInterval)
}

// SetFlip stores the flip flag. Vertical flip for NVENC would require a
// separate preprocessing step; it is a no-op here unless the caller handles
// it at the YUV level before passing frames in.
func (e *NVENC) SetFlip(b bool) {
	e.flipped = b
}

// Shutdown frees all FFmpeg/CUDA resources held by the encoder.
func (e *NVENC) Shutdown() error {
	if e.ctx != nil {
		C.nvenc_destroy(e.ctx)
		e.ctx = nil
	}
	return nil
}

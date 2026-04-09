//go:build nvenc

package nvenc

/*
#cgo pkg-config: libavcodec libavutil
#include "nvenc_ctx.h"
#include <libavutil/opt.h>
#include <libavutil/hwcontext.h>
#include <libavutil/hwcontext_cuda.h>
#include <libavutil/pixfmt.h>
#include <stdlib.h>
#include <string.h>

// nvenc_new allocates and opens an h264_nvenc encoder.
// Returns NULL and sets err_msg (static buffer) on failure.
static const char *last_err = "";
nvenc_ctx *nvenc_new(int width, int height, int bitrate_kbps, const char *preset, const char *tune, int zero_copy) {
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
    {
        AVHWDeviceContext *hwdev = (AVHWDeviceContext *)ctx->hw_device_ctx->data;
        AVCUDADeviceContext *cuda_hw = (AVCUDADeviceContext *)hwdev->hwctx;
        ctx->cu_ctx = cuda_hw->cuda_ctx;
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
    ctx->codec_ctx->gop_size = 30;
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
    av_opt_set(ctx->codec_ctx->priv_data, "profile", "baseline", 0);

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
	"log"
	"sync/atomic"
	"unsafe"
)

var nvencDiagFrame int64

// NVENC implements the encoder.Encoder interface using FFmpeg's h264_nvenc.
// Build with the `nvenc` build tag to include this encoder.
type NVENC struct {
	ctx     *C.nvenc_ctx
	width   int
	height  int
	preset  string
	tune    string
	bitrate int
	flipped bool
}

// Options configures the NVENC encoder.
type Options struct {
	// Bitrate in kbps for CBR mode (default: 4000)
	Bitrate int
	// NVENC preset: p1 (fastest) … p7 (best quality). Default: "p4"
	Preset string
	// NVENC tune: ll (low latency), ull (ultra low latency), hq (high quality). Default: "ll"
	Tune string
	// ZeroCopy: if true, use NV12 sw_format for hw_frames_ctx (zero-copy path
	// writes NV12 directly via PTX kernels). If false, use YUV420P (CPU upload path).
	ZeroCopy bool
}

// NewEncoder creates a new NVENC encoder for the given dimensions.
func NewEncoder(w, h int, opts *Options) (*NVENC, error) {
	if opts == nil {
		opts = &Options{
			Bitrate: 15000,
			Preset:  "p5",
			Tune:    "ull",
		}
	}
	if opts.Bitrate <= 0 {
		opts.Bitrate = 15000
	}
	if opts.Preset == "" {
		opts.Preset = "p5"
	}
	if opts.Tune == "" {
		opts.Tune = "ull"
	}

	preset := C.CString(opts.Preset)
	tune := C.CString(opts.Tune)
	defer C.free(unsafe.Pointer(preset))
	defer C.free(unsafe.Pointer(tune))

	zcFlag := C.int(0)
	if opts.ZeroCopy {
		zcFlag = C.int(1)
	}
	ctx := C.nvenc_new(C.int(w), C.int(h), C.int(opts.Bitrate), preset, tune, zcFlag)
	if ctx == nil {
		errMsg := C.GoString(C.nvenc_last_error())
		return nil, fmt.Errorf("nvenc: %s", errMsg)
	}

	return &NVENC{
		ctx:     ctx,
		width:   w,
		height:  h,
		preset:  opts.Preset,
		tune:    opts.Tune,
		bitrate: opts.Bitrate,
	}, nil
}

// Encode encodes a single YUV I420 frame and returns H264 NAL units.
// The input slice must be width*height*3/2 bytes: [Y][U][V].
func (e *NVENC) Encode(yuv []byte) []byte {
	if len(yuv) == 0 {
		return nil
	}

	// Vertical flip for GL cores (OpenGL renders bottom-up).
	// x264 handles this natively via X264_CSP_VFLIP; NVENC has no equivalent,
	// so we reverse the row order of each I420 plane before upload.
	if e.flipped {
		flipI420(yuv, e.width, e.height)
	}

	// DIAG: log input/output sizes every 60 frames
	n := atomic.AddInt64(&nvencDiagFrame, 1)
	diagThisFrame := n%60 == 1

	var outSize C.int
	data := C.nvenc_encode(e.ctx, (*C.uint8_t)(unsafe.SliceData(yuv)), &outSize)
	if data == nil || outSize == 0 {
		if diagThisFrame {
			log.Printf("[cloudplay diag] nvenc.Encode frame=%d input_len=%d output=nil/0 (EAGAIN or error)", n, len(yuv))
		}
		return nil
	}
	// Copy out before the next encode call overwrites the packet buffer.
	result := make([]byte, int(outSize))
	copy(result, unsafe.Slice((*byte)(unsafe.Pointer(data)), int(outSize)))

	if diagThisFrame {
		log.Printf("[cloudplay diag] nvenc.Encode frame=%d input_len=%d output_len=%d", n, len(yuv), len(result))
	}
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
	return fmt.Sprintf("h264_nvenc (preset=%s, tune=%s, bitrate=%dkbps)", e.preset, e.tune, e.bitrate)
}

// SetFlip enables vertical flipping of frames before encoding.
// GL-based cores render with a bottom-left origin, producing upside-down
// frames. This flag causes Encode to reverse row order in the I420 planes
// before uploading to NVENC.
func (e *NVENC) SetFlip(b bool) {
	e.flipped = b
}

// flipI420 reverses the row order of each plane in an I420 buffer in-place.
// Y plane: width × height, U plane: width/2 × height/2, V plane: same as U.
func flipI420(yuv []byte, width, height int) {
	ySize := width * height
	uvW := width >> 1
	uvH := height >> 1

	// Flip Y plane
	flipPlane(yuv[:ySize], width, height)
	// Flip U plane
	flipPlane(yuv[ySize:ySize+uvW*uvH], uvW, uvH)
	// Flip V plane
	flipPlane(yuv[ySize+uvW*uvH:], uvW, uvH)
}

// flipPlane reverses row order in a contiguous plane buffer in-place.
func flipPlane(plane []byte, stride, rows int) {
	tmp := make([]byte, stride)
	for top, bot := 0, rows-1; top < bot; top, bot = top+1, bot-1 {
		topOff := top * stride
		botOff := bot * stride
		copy(tmp, plane[topOff:topOff+stride])
		copy(plane[topOff:topOff+stride], plane[botOff:botOff+stride])
		copy(plane[botOff:botOff+stride], tmp)
	}
}

// Shutdown frees all FFmpeg/CUDA resources held by the encoder.
func (e *NVENC) Shutdown() error {
	if e.ctx != nil {
		C.nvenc_destroy(e.ctx)
		e.ctx = nil
	}
	return nil
}

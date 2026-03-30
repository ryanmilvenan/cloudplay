// nvenc_ctx.h — shared C definition for nvenc_ctx.
//
// Included by nvenc.go and nvenc_cuda.go preambles so that the struct
// layout is consistent across both CGO translation units.
//
// This file is NOT a Go file and must not have a build tag.
#pragma once

#include <libavcodec/avcodec.h>

// nvenc_ctx holds all FFmpeg state for one h264_nvenc encoding session.
typedef struct {
    AVCodecContext *codec_ctx;
    AVFrame        *frame;
    AVPacket       *packet;
    AVBufferRef    *hw_device_ctx;
    int             width;
    int             height;
    int             y_size;   // luma plane size (w*h)
    int             uv_size;  // each chroma plane size (w*h/4)
    int64_t         pts;
} nvenc_ctx;

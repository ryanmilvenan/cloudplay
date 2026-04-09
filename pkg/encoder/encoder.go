package encoder

import (
	"fmt"
	"log"
	"sync/atomic"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/encoder/h264"
	"github.com/giongto35/cloud-game/v3/pkg/encoder/nvenc"
	"github.com/giongto35/cloud-game/v3/pkg/encoder/vpx"
	"github.com/giongto35/cloud-game/v3/pkg/encoder/yuv"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

var encoderDiagFrame int64

type (
	InFrame  yuv.RawFrame
	OutFrame []byte
	Encoder  interface {
		Encode([]byte) []byte
		IntraRefresh()
		Info() string
		SetFlip(bool)
		Shutdown() error
	}
)

type Video struct {
	codec   Encoder
	log     *logger.Logger
	stopped atomic.Bool
	y       yuv.Conv
	pf      yuv.PixFmt
	rot     uint
}

type VideoCodec string

const (
	H264     VideoCodec = "h264"
	H264NVENC VideoCodec = "h264_nvenc"
	VP8      VideoCodec = "vp8"
	VP9      VideoCodec = "vp9"
	VPX      VideoCodec = "vpx"
)

// NewVideoEncoder returns new video encoder.
// By default, it waits for RGBA images on the input channel,
// converts them into YUV I420 format,
// encodes with provided video encoder, and
// puts the result into the output channel.
func NewVideoEncoder(w, h, dw, dh int, scale float64, conf config.Video, log *logger.Logger) (*Video, error) {
	var enc Encoder
	var err error
	codec := VideoCodec(conf.Codec)
	switch codec {
	case H264:
		opts := h264.Options(conf.H264)
		enc, err = h264.NewEncoder(dw, dh, conf.Threads, &opts)
	case H264NVENC:
		opts := nvenc.Options{
			Bitrate: conf.Nvenc.Bitrate,
			Preset:  conf.Nvenc.Preset,
			Tune:    conf.Nvenc.Tune,
		}
		enc, err = nvenc.NewEncoder(dw, dh, &opts)
	case VP8, VP9, VPX:
		opts := vpx.Options(conf.Vpx)
		v := 8
		if codec == VP9 {
			v = 9
		}
		enc, err = vpx.NewEncoder(dw, dh, conf.Threads, v, &opts)
	default:
		err = fmt.Errorf("unsupported codec: %v", conf.Codec)
	}
	if err != nil {
		return nil, err
	}
	if enc == nil {
		return nil, fmt.Errorf("no encoder")
	}

	return &Video{codec: enc, y: yuv.NewYuvConv(w, h, scale), log: log}, nil
}

func (v *Video) Encode(frame InFrame) OutFrame {
	if v.stopped.Load() {
		return nil
	}

	yCbCr := v.y.Process(yuv.RawFrame(frame), v.rot, v.pf)
	//defer v.y.Put(&yCbCr)

	// DIAG: log codec identity, YUV conversion, and check if input is all-black
	n := atomic.AddInt64(&encoderDiagFrame, 1)
	if n%60 == 1 {
		nonZero := 0
		for i := 0; i < len(frame.Data) && i < 1000; i++ {
			if frame.Data[i] != 0 { nonZero++; break }
		}
		yuvNonZero := 0
		for i := 0; i < len(yCbCr) && i < 1000; i++ {
			if yCbCr[i] != 0 { yuvNonZero++; break }
		}
		log.Printf("[cloudplay diag] Video.Encode frame=%d codec=%T yuv_len=%d input_data_len=%d pf=%d rot=%d inputHasData=%v yuvHasData=%v first8input=%v first8yuv=%v",
			n, v.codec, len(yCbCr), len(frame.Data), v.pf, v.rot, nonZero > 0, yuvNonZero > 0,
			frame.Data[:min(8, len(frame.Data))], yCbCr[:min(8, len(yCbCr))])
	}

	if bytes := v.codec.Encode(yCbCr); len(bytes) > 0 {
		if n%60 == 1 {
			log.Printf("[cloudplay diag] Video.Encode frame=%d codec returned %d bytes", n, len(bytes))
		}
		return bytes
	}

	if n%60 == 1 {
		log.Printf("[cloudplay diag] Video.Encode frame=%d codec returned nil/empty", n)
	}
	return nil
}

func (v *Video) Info() string {
	return fmt.Sprintf("%v, libyuv: %v", v.codec.Info(), v.y.Version())
}

func (v *Video) SetPixFormat(f uint32) {
	if v == nil {
		return
	}

	switch f {
	case 0:
		v.pf = yuv.PixFmt(yuv.FourccRgb0)
	case 1:
		v.pf = yuv.PixFmt(yuv.FourccArgb)
	case 2:
		v.pf = yuv.PixFmt(yuv.FourccRgbp)
	default:
		v.pf = yuv.PixFmt(yuv.FourccAbgr)
	}
}

// SetRot sets the de-rotation angle of the frames.
func (v *Video) SetRot(a uint) {
	if v == nil {
		return
	}

	if a > 0 {
		v.rot = (a + 180) % 360
	}
}

// SetFlip tells the encoder to flip the frames vertically.
func (v *Video) SetFlip(b bool) {
	if v == nil {
		return
	}
	v.codec.SetFlip(b)
}

func (v *Video) Stop() {
	if v == nil {
		return
	}

	if v.stopped.Swap(true) {
		return
	}
	v.rot = 0

	defer func() { v.codec = nil }()
	if err := v.codec.Shutdown(); err != nil {
		if v.log != nil {
			v.log.Error().Err(err).Msg("failed to close the encoder")
		}
	}
}

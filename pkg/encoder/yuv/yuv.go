package yuv

import (
	"image"

	"github.com/giongto35/cloud-game/v3/pkg/encoder/yuv/libyuv"
)

type Conv struct {
	w, h    int
	sw, sh  int
	scale   float64
	frame   []byte
	frameSc []byte
}

type RawFrame struct {
	Data   []byte
	Stride int
	W, H   int
}

type PixFmt uint32

const FourccRgbp = libyuv.FourccRgbp
const FourccArgb = libyuv.FourccArgb
const FourccAbgr = libyuv.FourccAbgr
const FourccRgb0 = libyuv.FourccRgb0

func yuvBufSize(w, h int) int {
	cw := (w + 1) / 2
	ch := (h + 1) / 2
	return w*h + 2*cw*ch
}

func NewYuvConv(w, h int, scale float64) Conv {
	if scale < 1 {
		scale = 1
	}

	sw, sh := round(w, scale), round(h, scale)
	conv := Conv{w: w, h: h, sw: sw, sh: sh, scale: scale}
	// Use integer math matching I420 plane layout — float w*h*1.5 truncates for odd dimensions.
	bufSize := yuvBufSize(w, h)

	if scale == 1 {
		conv.frame = make([]byte, bufSize)
	} else {
		bufSizeSc := yuvBufSize(sw, sh)
		// [original frame][scaled frame          ]
		frames := make([]byte, bufSize+bufSizeSc)
		conv.frame = frames[:bufSize]
		conv.frameSc = frames[bufSize:]
	}

	return conv
}

// Process converts an image to YUV I420 format inside the internal buffer.
func (c *Conv) Process(frame RawFrame, rot uint, pf PixFmt) []byte {
	cx, cy := c.w, c.h // crop
	if rot == 90 || rot == 270 {
		cx, cy = cy, cx
	}

	var stride int
	switch pf {
	case PixFmt(libyuv.FourccRgbp), PixFmt(libyuv.FourccRgb0):
		stride = frame.Stride >> 1
	default:
		stride = frame.Stride >> 2
	}

	if rot == 0 && (pf == PixFmt(libyuv.FourccAbgr) || pf == PixFmt(libyuv.FourccArgb)) {
		rgbaLikeToI420(frame.Data, c.frame, frame.Stride, cx, cy, pf)
	} else {
		libyuv.Y420(frame.Data, c.frame, frame.W, frame.H, stride, c.w, c.h, rot, uint32(pf), cx, cy)
	}

	if c.scale > 1 {
		libyuv.Y420Scale(c.frame, c.frameSc, c.w, c.h, c.sw, c.sh)
		return c.frameSc
	}

	return c.frame
}

func rgbaLikeToI420(src []byte, dst []byte, srcStrideBytes int, w, h int, pf PixFmt) {
	cw := (w + 1) / 2
	ch := (h + 1) / 2
	i0 := w * h
	i1 := i0 + cw*ch
	yPlane := dst[:i0]
	uPlane := dst[i0:i1]
	vPlane := dst[i1 : i1+cw*ch]

	for by := 0; by < h; by += 2 {
		for bx := 0; bx < w; bx += 2 {
			uAcc, vAcc, n := 0, 0, 0
			for dy := 0; dy < 2 && by+dy < h; dy++ {
				row := (by+dy)*srcStrideBytes
				for dx := 0; dx < 2 && bx+dx < w; dx++ {
					off := row + (bx+dx)*4
					if off+3 >= len(src) {
						continue
					}
					var r, g, b int
					switch pf {
					case PixFmt(libyuv.FourccAbgr):
						// ABGR fourcc is RGBA in memory.
						r = int(src[off+0])
						g = int(src[off+1])
						b = int(src[off+2])
					case PixFmt(libyuv.FourccArgb):
						// ARGB fourcc is BGRA in memory.
						b = int(src[off+0])
						g = int(src[off+1])
						r = int(src[off+2])
					default:
						r = int(src[off+0])
						g = int(src[off+1])
						b = int(src[off+2])
					}
					yPlane[(by+dy)*w+(bx+dx)] = clamp8(((66*r + 129*g + 25*b + 128) >> 8) + 16)
					uAcc += (((-38*r - 74*g + 112*b + 128) >> 8) + 128)
					vAcc += (((112*r - 94*g - 18*b + 128) >> 8) + 128)
					n++
				}
			}
			if n > 0 {
				uvIdx := (by/2)*cw + (bx / 2)
				uPlane[uvIdx] = clamp8(uAcc / n)
				vPlane[uvIdx] = clamp8(vAcc / n)
			}
		}
	}
}

func clamp8(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func (c *Conv) Version() string      { return libyuv.Version() }
func round(x int, scale float64) int { return (int(float64(x)*scale) + 1) & ^1 }

func ToYCbCr(bytes []byte, w, h int) *image.YCbCr {
	cw, ch := (w+1)/2, (h+1)/2

	i0 := w*h + 0*cw*ch
	i1 := w*h + 1*cw*ch
	i2 := w*h + 2*cw*ch

	yuv := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	yuv.Y = bytes[:i0:i0]
	yuv.Cb = bytes[i0:i1:i1]
	yuv.Cr = bytes[i1:i2:i2]
	return yuv
}

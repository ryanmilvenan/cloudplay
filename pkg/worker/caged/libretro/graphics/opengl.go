package graphics

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sync/atomic"
	"unsafe"

	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/libretro/graphics/gl"
)

type Context int

const (
	CtxNone Context = iota
	CtxOpenGl
	CtxOpenGlEs2
	CtxOpenGlCore
	CtxOpenGlEs3
	CtxOpenGlEsVersion
	CtxVulkan
	CtxUnknown = math.MaxInt32 - 1
	CtxDummy   = math.MaxInt32
)

type PixelFormat int

const (
	UnsignedShort5551 PixelFormat = iota
	UnsignedShort565
	UnsignedInt8888Rev
)

var (
	fbo, tex, rbo      uint32
	hasDepth           bool
	pixType, pixFormat uint32
	buf                []byte
	bufPtr             unsafe.Pointer
	glProcAddrFunc     func(name string) unsafe.Pointer
	lastCallbackDrawFbo atomic.Int32
	lastCallbackReadFbo atomic.Int32
)

func initContext(getProcAddr func(name string) unsafe.Pointer) {
	glProcAddrFunc = getProcAddr
	if err := gl.InitWithProcAddrFunc(getProcAddr); err != nil {
		panic(err)
	}
	gl.PixelStorei(gl.PackAlignment, 1)
}

func initFramebuffer(width, height int, depth, stencil bool) error {
	w, h := int32(width), int32(height)
	hasDepth = depth

	gl.GenTextures(1, &tex)
	gl.BindTexture(gl.Texture2d, tex)
	gl.TexParameteri(gl.Texture2d, gl.TextureMinFilter, gl.NEAREST)
	gl.TexParameteri(gl.Texture2d, gl.TextureMagFilter, gl.NEAREST)
	gl.TexImage2D(gl.Texture2d, 0, gl.RGBA8, w, h, 0, pixType, pixFormat, nil)
	gl.BindTexture(gl.Texture2d, 0)

	gl.GenFramebuffers(1, &fbo)
	gl.BindFramebuffer(gl.FRAMEBUFFER, fbo)
	gl.FramebufferTexture2D(gl.FRAMEBUFFER, gl.ColorAttachment0, gl.Texture2d, tex, 0)

	if depth {
		gl.GenRenderbuffers(1, &rbo)
		gl.BindRenderbuffer(gl.RENDERBUFFER, rbo)
		format, attachment := uint32(gl.DepthComponent24), uint32(gl.DepthAttachment)
		if stencil {
			format, attachment = gl.Depth24Stencil8, gl.DepthStencilAttachment
		}
		gl.RenderbufferStorage(gl.RENDERBUFFER, format, w, h)
		gl.FramebufferRenderbuffer(gl.FRAMEBUFFER, attachment, gl.RENDERBUFFER, rbo)
		gl.BindRenderbuffer(gl.RENDERBUFFER, 0)
	}

	if status := gl.CheckFramebufferStatus(gl.FRAMEBUFFER); status != gl.FramebufferComplete {
		return fmt.Errorf("framebuffer incomplete: 0x%X", status)
	}

	// glsm-style: tell the C wrapper layer that this is the frontend FBO,
	// and enable FBO 0 → frontend FBO remapping so the core's "default
	// framebuffer" writes land in our readback target.
	gl.CoreSetFrontendFbo(fbo)
	gl.CoreSetFbo0Remap(true)
	fmt.Fprintf(os.Stderr, "[glsm] initFramebuffer: frontendFbo=%d tex=%d rbo=%d depth=%v stencil=%v\n",
		fbo, tex, rbo, depth, stencil)

	return nil
}

func destroyFramebuffer() {
	gl.CoreSetFbo0Remap(false)
	if hasDepth {
		gl.DeleteRenderbuffers(1, &rbo)
	}
	gl.DeleteFramebuffers(1, &fbo)
	gl.DeleteTextures(1, &tex)
}

var readFbDiagN int64

func CaptureHwFramebufferBindings() (drawFbo, readFbo int32) {
	gl.GetIntegerv(gl.FramebufferBinding, &drawFbo)
	gl.GetIntegerv(gl.ReadFramebufferBinding, &readFbo)
	lastCallbackDrawFbo.Store(drawFbo)
	lastCallbackReadFbo.Store(readFbo)
	return drawFbo, readFbo
}

func ReadFramebuffer(size, w, h uint) []byte {
	boundFbo := int32(0)
	gl.GetIntegerv(gl.FramebufferBinding, &boundFbo)
	readBoundFbo := int32(0)
	gl.GetIntegerv(gl.ReadFramebufferBinding, &readBoundFbo)
	cachedDrawFbo := lastCallbackDrawFbo.Load()
	cachedReadFbo := lastCallbackReadFbo.Load()
	trackedPrivateDrawFbo := gl.CoreLastPrivateDrawFbo()

	// --- Experiment A: FBO-to-FBO blit from last-private (existing) ---
	blitResult := int32(-99)
	if trackedPrivateDrawFbo > 0 {
		blitResult = gl.CoreBlitToFrontend(uint32(trackedPrivateDrawFbo), fbo, int32(w), int32(h))
	}
	gl.BindFramebuffer(gl.FRAMEBUFFER, fbo)
	gl.ReadPixels(0, 0, int32(w), int32(h), pixType, pixFormat, bufPtr)
	blitNonZero := countNonZero(buf, int(size))

	// --- Experiment B: Direct ReadPixels from core's private FBO (no blit) ---
	directNonZero := 0
	if trackedPrivateDrawFbo > 0 {
		gl.BindFramebuffer(gl.FRAMEBUFFER, uint32(trackedPrivateDrawFbo))
		gl.ReadPixels(0, 0, int32(w), int32(h), pixType, pixFormat, bufPtr)
		directNonZero = countNonZero(buf, int(size))
	}

	// --- Experiment C: Candidate scan — try tex-attach from ALL tracked FBO textures ---
	candidates := gl.CoreGetCandidates()
	winnerIdx := -1           // index into candidates that yielded pixels
	winnerNonZero := 0
	scanResults := make([]int, len(candidates)) // nonzero count per candidate
	for ci, cand := range candidates {
		res := gl.CoreReadbackViaTexAttach(cand.TexID, fbo, tex)
		if res == 0 {
			gl.ReadPixels(0, 0, int32(w), int32(h), pixType, pixFormat, bufPtr)
			nz := countNonZero(buf, int(size))
			scanResults[ci] = nz
			if nz > 0 && winnerIdx < 0 {
				winnerIdx = ci
				winnerNonZero = nz
			}
		} else {
			scanResults[ci] = -1 // attach failed
		}
		gl.CoreRestoreFrontendTex(fbo, tex)
	}

	// --- Pick best result ---
	if winnerIdx >= 0 {
		// Re-read from the winning candidate (last read may have been a later candidate)
		cand := candidates[winnerIdx]
		gl.CoreReadbackViaTexAttach(cand.TexID, fbo, tex)
		gl.ReadPixels(0, 0, int32(w), int32(h), pixType, pixFormat, bufPtr)
		gl.CoreRestoreFrontendTex(fbo, tex)
	} else if directNonZero > 0 {
		gl.BindFramebuffer(gl.FRAMEBUFFER, uint32(trackedPrivateDrawFbo))
		gl.ReadPixels(0, 0, int32(w), int32(h), pixType, pixFormat, bufPtr)
	} else if blitNonZero > 0 {
		gl.BindFramebuffer(gl.FRAMEBUFFER, fbo)
		gl.ReadPixels(0, 0, int32(w), int32(h), pixType, pixFormat, bufPtr)
	}
	// else: all black

	n := readFbDiagN
	readFbDiagN++
	if n < 10 || n%300 == 0 {
		fmt.Fprintf(os.Stderr, "[DIAG ReadFramebuffer] n=%d cloudplayFbo=%d boundFbo=%d readBoundFbo=%d cachedDraw=%d cachedRead=%d privDrawFbo=%d blitResult=%d w=%d h=%d size=%d\n",
			n, fbo, boundFbo, readBoundFbo, cachedDrawFbo, cachedReadFbo, trackedPrivateDrawFbo, blitResult, w, h, size)
		fmt.Fprintf(os.Stderr, "[DIAG ReadFramebuffer] n=%d RESULTS: blitNonZero=%d directNonZero=%d candidates=%d winnerIdx=%d winnerNonZero=%d\n",
			n, blitNonZero, directNonZero, len(candidates), winnerIdx, winnerNonZero)
		for ci, cand := range candidates {
			fmt.Fprintf(os.Stderr, "[DIAG ReadFramebuffer]   candidate[%d] fbo=%d tex=%d %dx%d ifmt=0x%X nonzero=%d\n",
				ci, cand.FboID, cand.TexID, cand.TexWidth, cand.TexHeight, cand.TexInternalFmt, scanResults[ci])
		}
		gl.CoreDumpFboTable()
		gl.CoreDumpTexTable()
	}
	return buf[:size]
}

func countNonZero(data []byte, size int) int {
	count := 0
	limit := size
	if limit > 2000 {
		limit = 2000
	}
	for i := 0; i < limit; i++ {
		if data[i] != 0 {
			count++
			if count > 5 {
				return count
			}
		}
	}
	return count
}

func SetBuffer(size int) {
	buf = make([]byte, size)
	bufPtr = unsafe.Pointer(&buf[0])
}

func SetPixelFormat(format PixelFormat) error {
	switch format {
	case UnsignedShort5551:
		pixFormat, pixType = gl.UnsignedShort5551, gl.BGRA
	case UnsignedShort565:
		pixFormat, pixType = gl.UnsignedShort565, gl.RGB
	case UnsignedInt8888Rev:
		pixFormat, pixType = gl.UnsignedInt8888Rev, gl.BGRA
	default:
		return errors.New("unknown pixel format")
	}
	return nil
}

func GLInfo() (version, vendor, renderer, glsl string) {
	return gl.GoStr(gl.GetString(gl.VERSION)),
		gl.GoStr(gl.GetString(gl.VENDOR)),
		gl.GoStr(gl.GetString(gl.RENDERER)),
		gl.GoStr(gl.GetString(gl.ShadingLanguageVersion))
}

func GlFbo() uint32 { return fbo }

var glProcDiagN int64

func GlProcAddress(proc string) unsafe.Pointer {
	if glProcAddrFunc == nil {
		return nil
	}
	ptr := glProcAddrFunc(proc)
	if ptr != nil && (proc == "glBindFramebuffer" || proc == "glBindFramebufferEXT" || proc == "glBlitFramebuffer" || proc == "glBlitFramebufferEXT" || proc == "glFramebufferTexture2D" || proc == "glFramebufferTexture2DEXT" || proc == "glFramebufferRenderbuffer" || proc == "glFramebufferRenderbufferEXT" || proc == "glGenFramebuffers" || proc == "glGenFramebuffersEXT" || proc == "glBindTexture" || proc == "glTexImage2D" || proc == "glTexStorage2D") {
		n := glProcDiagN
		glProcDiagN++
		if n < 20 {
			fmt.Fprintf(os.Stderr, "[DIAG coreGetProcAddress] n=%d sym=%s ptr=%p\n", n, proc, ptr)
		}
	}
	return gl.CoreWrapProcAddress(proc, ptr)
}

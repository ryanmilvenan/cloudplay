//go:build linux

package graphics

/*
#cgo linux pkg-config: egl
#cgo linux LDFLAGS: -ldl
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <dlfcn.h>
#include <stdlib.h>

static void* cloudplay_egl_get_proc_address(const char* name) {
	void* p = (void*)eglGetProcAddress(name);
	if (!p) p = dlsym(RTLD_DEFAULT, name);
	return p;
}

static EGLDisplay cloudplay_egl_get_display_default(void) {
	return eglGetDisplay(EGL_DEFAULT_DISPLAY);
}

static EGLContext cloudplay_egl_create_context_no_share(EGLDisplay dpy, EGLConfig cfg, EGLint *attrs) {
	return eglCreateContext(dpy, cfg, EGL_NO_CONTEXT, attrs);
}

static void cloudplay_egl_clear_current(EGLDisplay dpy) {
	eglMakeCurrent(dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, EGL_NO_CONTEXT);
}

static int cloudplay_egl_is_no_display(EGLDisplay dpy) {
	return dpy == EGL_NO_DISPLAY;
}

static int cloudplay_egl_is_no_surface(EGLSurface surface) {
	return surface == EGL_NO_SURFACE;
}

static int cloudplay_egl_is_no_context(EGLContext ctx) {
	return ctx == EGL_NO_CONTEXT;
}

static EGLDisplay cloudplay_egl_no_display(void) {
	return EGL_NO_DISPLAY;
}

static EGLSurface cloudplay_egl_no_surface(void) {
	return EGL_NO_SURFACE;
}

static EGLContext cloudplay_egl_no_context(void) {
	return EGL_NO_CONTEXT;
}
*/
import "C"
import (
	"fmt"
	"os"
	"unsafe"
)

type EGL struct {
	dpy     C.EGLDisplay
	surface C.EGLSurface
	ctx     C.EGLContext
}

func NewEGLContext(cfg Config) (*EGL, error) {
	dpy := C.cloudplay_egl_get_display_default()
	if C.cloudplay_egl_is_no_display(dpy) != 0 {
		return nil, fmt.Errorf("eglGetDisplay failed")
	}

	var major, minor C.EGLint
	if C.eglInitialize(dpy, &major, &minor) == C.EGL_FALSE {
		return nil, fmt.Errorf("eglInitialize failed: 0x%x", uint32(C.eglGetError()))
	}

	api := C.EGLenum(C.EGL_OPENGL_API)
	renderableType := C.EGLint(C.EGL_OPENGL_BIT)
	ctxAttrs := []C.EGLint{C.EGL_NONE}

	switch cfg.Ctx {
	case CtxOpenGl, CtxOpenGlCore:
		api = C.EGL_OPENGL_API
		renderableType = C.EGL_OPENGL_BIT
		ctxAttrs = []C.EGLint{
			C.EGL_CONTEXT_MAJOR_VERSION, C.EGLint(cfg.GLVersionMajor),
			C.EGL_CONTEXT_MINOR_VERSION, C.EGLint(cfg.GLVersionMinor),
			C.EGL_NONE,
		}
	case CtxOpenGlEs2, CtxOpenGlEs3, CtxOpenGlEsVersion:
		api = C.EGL_OPENGL_ES_API
		renderableType = C.EGL_OPENGL_ES2_BIT
		clientVersion := 2
		if cfg.GLVersionMajor >= 3 {
			clientVersion = int(cfg.GLVersionMajor)
			renderableType = C.EGL_OPENGL_ES3_BIT_KHR
		}
		ctxAttrs = []C.EGLint{
			C.EGL_CONTEXT_CLIENT_VERSION, C.EGLint(clientVersion),
			C.EGL_NONE,
		}
	default:
		_ = C.eglTerminate(dpy)
		return nil, fmt.Errorf("egl: unsupported context type %v", cfg.Ctx)
	}

	if C.eglBindAPI(api) == C.EGL_FALSE {
		_ = C.eglTerminate(dpy)
		return nil, fmt.Errorf("eglBindAPI failed: 0x%x", uint32(C.eglGetError()))
	}

	attrs := []C.EGLint{
		C.EGL_SURFACE_TYPE, C.EGL_PBUFFER_BIT,
		C.EGL_RENDERABLE_TYPE, renderableType,
		C.EGL_RED_SIZE, 8,
		C.EGL_GREEN_SIZE, 8,
		C.EGL_BLUE_SIZE, 8,
		C.EGL_ALPHA_SIZE, 8,
		C.EGL_DEPTH_SIZE, boolToEGL(cfg.GLHasDepth, 24),
		C.EGL_STENCIL_SIZE, boolToEGL(cfg.GLHasStencil, 8),
		C.EGL_NONE,
	}

	var eglCfg C.EGLConfig
	var numCfg C.EGLint
	if C.eglChooseConfig(dpy, &attrs[0], &eglCfg, 1, &numCfg) == C.EGL_FALSE || numCfg == 0 {
		_ = C.eglTerminate(dpy)
		return nil, fmt.Errorf("eglChooseConfig failed: 0x%x", uint32(C.eglGetError()))
	}

	pbufferAttrs := []C.EGLint{
		C.EGL_WIDTH, 1,
		C.EGL_HEIGHT, 1,
		C.EGL_NONE,
	}
	surface := C.eglCreatePbufferSurface(dpy, eglCfg, &pbufferAttrs[0])
	if C.cloudplay_egl_is_no_surface(surface) != 0 {
		_ = C.eglTerminate(dpy)
		return nil, fmt.Errorf("eglCreatePbufferSurface failed: 0x%x", uint32(C.eglGetError()))
	}

	ctx := C.cloudplay_egl_create_context_no_share(dpy, eglCfg, &ctxAttrs[0])
	if C.cloudplay_egl_is_no_context(ctx) != 0 {
		C.eglDestroySurface(dpy, surface)
		_ = C.eglTerminate(dpy)
		return nil, fmt.Errorf("eglCreateContext failed: 0x%x", uint32(C.eglGetError()))
	}

	e := &EGL{dpy: dpy, surface: surface, ctx: ctx}
	if err := e.BindContext(); err != nil {
		_ = e.Deinit()
		return nil, err
	}

	initContext(func(name string) unsafe.Pointer {
		cname := C.CString(name)
		defer C.free(unsafe.Pointer(cname))
		return C.cloudplay_egl_get_proc_address(cname)
	})

	if err := initFramebuffer(cfg.W, cfg.H, cfg.GLHasDepth, cfg.GLHasStencil); err != nil {
		_ = e.Deinit()
		return nil, fmt.Errorf("egl fbo: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[cloudplay diag] EGL context ready api=%d gl=%d.%d depth=%v stencil=%v\n", int(api), cfg.GLVersionMajor, cfg.GLVersionMinor, cfg.GLHasDepth, cfg.GLHasStencil)
	return e, nil
}

func (e *EGL) BindContext() error {
	if C.eglMakeCurrent(e.dpy, e.surface, e.surface, e.ctx) == C.EGL_FALSE {
		return fmt.Errorf("eglMakeCurrent failed: 0x%x", uint32(C.eglGetError()))
	}
	return nil
}

func (e *EGL) Deinit() error {
	destroyFramebuffer()
	if e == nil {
		return nil
	}
	if C.cloudplay_egl_is_no_display(e.dpy) == 0 {
		C.cloudplay_egl_clear_current(e.dpy)
		if C.cloudplay_egl_is_no_context(e.ctx) == 0 {
			C.eglDestroyContext(e.dpy, e.ctx)
			e.ctx = C.cloudplay_egl_no_context()
		}
		if C.cloudplay_egl_is_no_surface(e.surface) == 0 {
			C.eglDestroySurface(e.dpy, e.surface)
			e.surface = C.cloudplay_egl_no_surface()
		}
		C.eglTerminate(e.dpy)
		e.dpy = C.cloudplay_egl_no_display()
	}
	return nil
}

func boolToEGL(ok bool, value C.EGLint) C.EGLint {
	if ok {
		return value
	}
	return 0
}

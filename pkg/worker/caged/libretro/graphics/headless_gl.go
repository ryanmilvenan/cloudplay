package graphics

import (
	"fmt"
	"os"
)

// HeadlessGLContext is the backend seam for non-Vulkan GPU rendering.
//
// Today the implementation is SDL-backed by default. A Linux-only EGL pbuffer
// path can be selected explicitly for headless GPU-native OpenGL testing
// without changing the shared media pipeline.
type HeadlessGLContext interface {
	BindContext() error
	Deinit() error
}

// NewHeadlessGLContext returns the active OpenGL context backend.
//
// Selection is intentionally conservative:
//   - default / unset: SDL hidden window (existing behavior)
//   - CLOUDPLAY_GL_BACKEND=egl: try EGL first, then fall back to SDL on error
//
// This keeps current live behavior intact while allowing incremental headless
// EGL validation on Linux.
func NewHeadlessGLContext(cfg Config) (HeadlessGLContext, error) {
	backend := os.Getenv("CLOUDPLAY_GL_BACKEND")
	if backend == "egl" {
		if ctx, err := newRequestedHeadlessGLContext(cfg, backend); err == nil {
			fmt.Fprintf(os.Stderr, "[cloudplay diag] headless GL backend selected: egl\n")
			return ctx, nil
		} else {
			fmt.Fprintf(os.Stderr, "[cloudplay diag] headless GL backend egl unavailable, falling back to sdl: %v\n", err)
		}
	}
	fmt.Fprintf(os.Stderr, "[cloudplay diag] headless GL backend selected: sdl\n")
	return NewSDLContext(cfg)
}

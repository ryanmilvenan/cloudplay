package app

// RenderBackendKind identifies the active frame-production backend behind an app.
//
// This intentionally describes the backend at a coarse level so the worker/media
// stack can choose backend-specific optimizations (for example Vulkan zero-copy)
// without depending on libretro- or renderer-specific types.
type RenderBackendKind string

const (
	RenderBackendSoftware RenderBackendKind = "software"
	RenderBackendOpenGL  RenderBackendKind = "opengl"
	RenderBackendVulkan  RenderBackendKind = "vulkan"
)

// VideoBackend exposes the active rendering/capture backend behind an app.
//
// Today this is used to keep the encode path shared while allowing backend-
// specific capabilities such as Vulkan zero-copy to be queried generically.
// The next backend slice can hang a headless EGL/OpenGL implementation behind
// the same seam without changing room/media orchestration.
type VideoBackend interface {
	Kind() RenderBackendKind
	Name() string
	SupportsZeroCopy() bool
	ZeroCopyFd(w, h uint) (int, uint64, error)
	WaitFrameReady() error
}

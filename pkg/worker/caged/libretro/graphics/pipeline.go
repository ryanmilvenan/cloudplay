package graphics

// CoreMeta holds the hardware context parameters that a core requests
// via RETRO_ENVIRONMENT_SET_HW_RENDER.
type CoreMeta struct {
	// Context type (e.g. CtxOpenGl, CtxVulkan).
	Ctx Context

	// Maximum render dimensions.
	MaxWidth  int
	MaxHeight int

	// GL-specific fields; ignored for Vulkan.
	GLAutoContext  bool
	GLVersionMajor uint
	GLVersionMinor uint
	GLHasDepth     bool
	GLHasStencil   bool
}

// RenderPipeline abstracts the underlying rendering back-end so that
// nanoarch.go can switch between GL (SDL-based) and Vulkan at runtime
// without ifdef-style branching.
//
// Lifecycle:
//   1. Init(meta) — called once after the core requests HW rendering.
//   2. ReadFrame(w, h) — called after each retro_run() to get pixel data.
//   3. Deinit() — called when the core or session is torn down.
type RenderPipeline interface {
	// Init sets up the pipeline for the given core metadata.
	// It should create any necessary GPU resources (FBO, staging buffer, etc.).
	Init(meta CoreMeta) error

	// ReadFrame returns the current rendered frame as a flat RGBA byte slice.
	// w and h are the actual render dimensions for this frame.
	ReadFrame(w, h uint) []byte

	// Deinit releases all pipeline resources.
	Deinit() error

	// IsVulkan returns true if the implementation uses Vulkan, false for GL.
	// Used by nanoarch.go to decide which environment callbacks to respond to.
	IsVulkan() bool
}

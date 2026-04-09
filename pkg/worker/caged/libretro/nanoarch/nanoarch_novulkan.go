//go:build !vulkan

package nanoarch

import "unsafe"

// vulkanState is a placeholder for builds without the vulkan tag.
// It mirrors the shape of the real vulkanState (nanoarch_vulkan.go) so that
// nanoarch.go can reference v.vulkan.enabled without build-tag branching.
// The enabled field is always false and ctx is always nil on !vulkan builds.
type vulkanState struct {
	enabled bool
	ctx     interface{} // placeholder — never set
}

// IsVulkan always returns false when Vulkan support is not compiled in.
func (n *Nanoarch) IsVulkan() bool { return false }

// preferredHWContextIsVulkan reports that Vulkan is not available on builds
// without the vulkan tag.
func preferredHWContextIsVulkan() bool { return false }

// handleVulkanHWRender returns false, signalling to the core that we cannot
// satisfy its Vulkan HW render request.  The GL path will remain active.
func handleVulkanHWRender(_ unsafe.Pointer) bool { return false }

// handleGetHWRenderInterface returns false; no Vulkan interface available.
func handleGetHWRenderInterface(_ unsafe.Pointer) bool { return false }

// initVulkanVideo is a no-op — the GL init path is used instead.
func initVulkanVideo() {}

// deinitVulkanVideo is a no-op.
func deinitVulkanVideo() {}

// readVulkanFramebuffer returns a zeroed frame; should never be called on
// non-Vulkan builds since Nan0.vulkan.enabled is always false.
func readVulkanFramebuffer(size, _, _ uint) []byte { return make([]byte, size) }

// vulkanZeroCopyFd always returns -1 on non-Vulkan builds.
func vulkanZeroCopyFd(_, _ uint) (int, uint64, error) {
	return -1, 0, nil
}

// IsZeroCopyAvailable always returns false on non-Vulkan builds.
func (n *Nanoarch) IsZeroCopyAvailable() bool { return false }

// ZeroCopyFd always returns -1 on non-Vulkan builds.
func (n *Nanoarch) ZeroCopyFd(w, h uint) (int, uint64, error) { return vulkanZeroCopyFd(w, h) }

// WaitZeroCopyBlit is a no-op on non-Vulkan builds.
func (n *Nanoarch) WaitZeroCopyBlit() error { return nil }

// handleSetHWRenderContextNegotiation returns false on non-Vulkan builds.
func handleSetHWRenderContextNegotiation(_ unsafe.Pointer) bool { return false }

// handleGetHWRenderContextNegotiationSupport returns false on non-Vulkan builds.
func handleGetHWRenderContextNegotiationSupport(_ unsafe.Pointer) bool { return false }

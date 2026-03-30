//go:build vulkan && linux && nvenc

package vulkan

// deviceExtensionsForExternalMemory returns the Vulkan device-extension names
// required for the Phase 3 zero-copy path (Vulkan external memory → CUDA).
//
// These are requested in addition to any base extensions when building with
// both the "linux" and "nvenc" tags, which is the only platform that supports
// VK_KHR_external_memory_fd.
func deviceExtensionsForExternalMemory() []string {
	return []string{
		"VK_KHR_external_memory",
		"VK_KHR_external_memory_fd",
	}
}

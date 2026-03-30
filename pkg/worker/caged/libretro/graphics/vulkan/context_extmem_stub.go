//go:build !(vulkan && linux && nvenc)

package vulkan

// deviceExtensionsForExternalMemory returns no extensions on platforms that
// do not support VK_KHR_external_memory_fd (non-Linux or non-NVENC builds).
func deviceExtensionsForExternalMemory() []string { return nil }

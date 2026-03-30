//go:build vulkan && linux && nvenc

package vulkan

// deviceExtensionsForExternalMemory returns the Vulkan device-extension names
// for Phase 3 zero-copy (VK_KHR_external_memory_fd).
//
// We keep this minimal because context.go falls back to NO extensions if any
// in this list are unavailable.  The Dolphin-required rendering extensions are
// handled via the negotiation interface (create_device2) where Dolphin itself
// requests what it needs.
func deviceExtensionsForExternalMemory() []string {
	return []string{
		"VK_KHR_external_memory",
		"VK_KHR_external_memory_fd",
	}
}

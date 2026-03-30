//go:build vulkan && !(linux && nvenc)

package vulkan

/*
#cgo LDFLAGS: -lvulkan
#include <vulkan/vulkan.h>
*/
import "C"
import "fmt"

// ZeroCopyBuffer stub for Vulkan builds on platforms that lack
// VK_KHR_external_memory_fd (non-Linux or non-NVENC).
//
// The type must match provider.go's usage exactly (same method signatures
// with CGo types) since both files are compiled together under the vulkan tag.

// ZeroCopyBuffer is a placeholder when external-memory is unavailable.
type ZeroCopyBuffer struct {
	width, height uint32
}

// NewZeroCopyBuffer always errors — external memory unavailable on this platform.
func NewZeroCopyBuffer(_ *Context, _, _ uint32) (*ZeroCopyBuffer, error) {
	return nil, fmt.Errorf("zerocopy: not available (requires linux+nvenc build tags)")
}

func (zc *ZeroCopyBuffer) Destroy()                    {}
func (zc *ZeroCopyBuffer) Size() uint64                 { return 0 }
func (zc *ZeroCopyBuffer) ExportMemoryFd() (int, error) { return -1, fmt.Errorf("zerocopy: not available") }

// BlitFrom is a no-op stub — zeroCopyAvailable() is always false on this platform
// so this method is never called at runtime.
func (zc *ZeroCopyBuffer) BlitFrom(_ C.VkImage, _ C.VkImageLayout) error {
	return fmt.Errorf("zerocopy: not available")
}

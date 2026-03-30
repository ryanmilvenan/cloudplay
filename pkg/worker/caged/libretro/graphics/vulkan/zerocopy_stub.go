//go:build !vulkan

package vulkan

import "fmt"

// ZeroCopyBuffer stubs — present on !vulkan builds.
// On vulkan builds without linux+nvenc, zerocopy_vulkan_stub.go is used instead.

// ZeroCopyBuffer is a placeholder on non-Vulkan builds.
type ZeroCopyBuffer struct{}

// NewZeroCopyBuffer always returns an error on non-Vulkan builds.
func NewZeroCopyBuffer(_ *Context, _, _ uint32) (*ZeroCopyBuffer, error) {
	return nil, fmt.Errorf("zerocopy: not available (requires -tags vulkan)")
}

func (zc *ZeroCopyBuffer) Destroy()                    {}
func (zc *ZeroCopyBuffer) Size() uint64                 { return 0 }
func (zc *ZeroCopyBuffer) ExportMemoryFd() (int, error) { return -1, fmt.Errorf("zerocopy: not available") }

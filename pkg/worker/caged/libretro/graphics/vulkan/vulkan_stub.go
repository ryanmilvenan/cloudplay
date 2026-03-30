//go:build !vulkan

// Package vulkan stubs out the Vulkan pipeline for builds without the
// vulkan build tag.  All constructors return an "unsupported" error so
// that callers can fall back to the GL pipeline gracefully.
package vulkan

import (
	"errors"
	"unsafe"
)

var ErrUnsupported = errors.New("vulkan: not compiled in (rebuild with -tags vulkan)")

// Config is a placeholder so code that references this package compiles
// without the vulkan tag.
type Config struct {
	Width  uint32
	Height uint32
}

// VulkanContext is a stub.
type VulkanContext struct{}

// NewVulkanContext always returns ErrUnsupported on non-vulkan builds.
func NewVulkanContext(_ Config) (*VulkanContext, error) { return nil, ErrUnsupported }

func (v *VulkanContext) ReadFramebuffer(size, w, h uint) []byte { return make([]byte, size) }
func (v *VulkanContext) RenderInterface() unsafe.Pointer        { return nil }
func (v *VulkanContext) Deinit() error                          { return nil }
func (v *VulkanContext) IsVulkan() bool                         { return false }
func (v *VulkanContext) ZeroCopyFd(_, _ uint) (int, error)      { return -1, ErrUnsupported }
func (v *VulkanContext) IsZeroCopyAvailable() bool              { return false }

//go:build vulkan

package vulkan

import (
	"os"
	"testing"
)

const (
	vkMemoryPropertyHostVisibleBit = 0x00000002
	vkMemoryPropertyHostCoherentBit = 0x00000004
)

// TestNewContext verifies that a headless Vulkan context can be created and
// destroyed cleanly.
//
// This test is skipped automatically on systems where no Vulkan ICD is present
// (the create call will fail and we treat that as "Vulkan unavailable" rather
// than a test failure).
func TestNewContext(t *testing.T) {
	ctx, err := NewContext()
	if err != nil {
		t.Skipf("Vulkan unavailable on this system: %v", err)
	}
	defer ctx.Destroy()

	if ctx.Instance == nil {
		t.Fatal("expected non-nil VkInstance")
	}
	if ctx.PhysDevice == nil {
		t.Fatal("expected non-nil VkPhysicalDevice")
	}
	if ctx.Device == nil {
		t.Fatal("expected non-nil VkDevice")
	}
	if ctx.Queue == nil {
		t.Fatal("expected non-nil VkQueue")
	}
	if ctx.CmdPool == nil {
		t.Fatal("expected non-nil VkCommandPool")
	}
}

// TestContextIdempotentDestroy verifies that calling Destroy() twice does not
// panic or cause a segfault.
func TestContextIdempotentDestroy(t *testing.T) {
	ctx, err := NewContext()
	if err != nil {
		t.Skipf("Vulkan unavailable: %v", err)
	}
	ctx.Destroy()
	ctx.Destroy() // second call must be a no-op
}

// TestFindMemoryType verifies the memory type helper returns valid results.
func TestFindMemoryType(t *testing.T) {
	ctx, err := NewContext()
	if err != nil {
		t.Skipf("Vulkan unavailable: %v", err)
	}
	defer ctx.Destroy()

	// Any non-zero typeFilter should find host-visible coherent memory on a
	// real Vulkan device.
	idx, err := ctx.findMemoryType(
		0xFFFFFFFF,
		vkMemoryPropertyHostVisibleBit|vkMemoryPropertyHostCoherentBit,
	)
	if err != nil {
		t.Fatalf("findMemoryType: %v", err)
	}
	if idx >= 32 {
		t.Fatalf("unexpected memory type index: %d", idx)
	}
}

// TestNewVulkanContext verifies the top-level public API.
func TestNewVulkanContext(t *testing.T) {
	vc, err := NewVulkanContext(Config{Width: 640, Height: 480})
	if err != nil {
		t.Skipf("Vulkan unavailable: %v", err)
	}
	defer vc.Deinit()

	if !vc.IsVulkan() {
		t.Fatal("IsVulkan() should return true")
	}
}

// TestMain checks for SKIP_VULKAN_TESTS env variable to allow CI to bypass
// all Vulkan tests when no GPU is available.
func TestMain(m *testing.M) {
	if os.Getenv("SKIP_VULKAN_TESTS") != "" {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

//go:build !(nvenc && linux && vulkan)

// Stub for TryArmZeroCopy on builds that lack nvenc+linux+vulkan.
// The zero-copy path is simply unavailable; returns false immediately.

package media

import (
	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// TryArmZeroCopy is a no-op stub.  Returns false — zero-copy requires
// -tags 'nvenc linux vulkan'.
func TryArmZeroCopy(
	_ *WebrtcMediaPipe,
	_ config.Video,
	_, _ uint,
	_ func(w, h uint) (int, uint64, error),
	_ func() error,
	log *logger.Logger,
) bool {
	log.Debug().Msg("media/zerocopy: not available (requires -tags 'nvenc linux vulkan')")
	return false
}

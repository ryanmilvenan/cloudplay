package room

import (
	"github.com/giongto35/cloud-game/v3/pkg/com"
	"github.com/giongto35/cloud-game/v3/pkg/network/webrtc"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/libretro"
)

type GameRouter struct {
	Router[*GameSession]
}

func NewGameRouter() *GameRouter {
	u := com.NewNetMap[SessionKey, *GameSession]()
	return &GameRouter{Router: Router[*GameSession]{users: &u}}
}

// WithEmulator returns the wrapped value as *libretro.Caged. Panics if the
// value is something else — callers must know the backend is libretro
// (check IsLibretro first, or guard against the xemu path before calling).
func WithEmulator(wtf any) *libretro.Caged { return wtf.(*libretro.Caged) }

// IsLibretro reports whether the given value is a libretro-backend cage
// (*libretro.Caged or one of its wrappers). Handlers for save/load/reset
// and other libretro-specific features use this to skip work when the
// room is running the xemu backend instead.
func IsLibretro(wtf any) bool {
	_, ok := wtf.(*libretro.Caged)
	return ok
}

func WithRecorder(wtf any) *libretro.RecordingFrontend {
	return (WithEmulator(wtf).Emulator).(*libretro.RecordingFrontend)
}
func WithWebRTC(wtf Session) *webrtc.Peer { return wtf.(*webrtc.Peer) }

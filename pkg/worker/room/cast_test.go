package room

import (
	"testing"

	"github.com/giongto35/cloud-game/v3/pkg/network/webrtc"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/libretro"
)

func TestGoodWithRecorder(t *testing.T) {
	WithRecorder(&libretro.Caged{Emulator: &libretro.RecordingFrontend{}})
}

func TestBadWithRecorder(t *testing.T) {
	defer func() { _ = recover() }()
	WithEmulator(libretro.Caged{})
	t.Errorf("no panic")
}

func TestGoodWithEmulator(t *testing.T) { WithEmulator(&libretro.Caged{}) }

func TestBadWithEmulator(t *testing.T) {
	defer func() { _ = recover() }()
	WithEmulator(libretro.Caged{}) // not a pointer
	t.Errorf("no panic")
}

func TestGoodWithWebRTCCast(t *testing.T) {
	WithWebRTC(GameSession{AppSession: AppSession{Session: &webrtc.Peer{}}}.Session)
}

func TestBadWithWebRTCCast(t *testing.T) {
	defer func() { _ = recover() }()
	WithWebRTC(GameSession{}) // not a Session due to deep nesting
	t.Errorf("no panic")
}

// TestIsLibretro is the Phase-6 backend-dispatch type guard. Handlers for
// save/load/reset check this before calling libretro-specific methods so
// the xemu backend can share the room/App surface without panicking the
// unsafe WithEmulator cast.
func TestIsLibretro(t *testing.T) {
	if !IsLibretro(&libretro.Caged{}) {
		t.Error("*libretro.Caged should be classified as libretro")
	}
	if IsLibretro(libretro.Caged{}) {
		t.Error("non-pointer libretro.Caged must NOT classify (unsafe cast would panic)")
	}
	if IsLibretro(nil) {
		t.Error("nil must not classify")
	}
	if IsLibretro("a string") {
		t.Error("arbitrary value must not classify")
	}
}

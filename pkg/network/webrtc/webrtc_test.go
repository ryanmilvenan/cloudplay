package webrtc

import (
	"testing"

	pion "github.com/pion/webrtc/v4"
)

func TestNewTrackMapsH264NVENCToH264Mime(t *testing.T) {
	track, err := newTrack("video", "video", "h264_nvenc")
	if err != nil {
		t.Fatalf("newTrack returned error: %v", err)
	}

	if got, want := track.Codec().MimeType, pion.MimeTypeH264; got != want {
		t.Fatalf("track mime = %q, want %q", got, want)
	}
}

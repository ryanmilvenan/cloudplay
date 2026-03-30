package webrtc

import (
	"testing"

	pion "github.com/pion/webrtc/v4"
)

func TestNormalizeCodec(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"h264", "h264"},
		{"H264_NVENC", "h264"},
		{" vp9 ", "vp9"},
	}

	for _, tt := range tests {
		if got := normalizeCodec(tt.in); got != tt.want {
			t.Fatalf("normalizeCodec(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNewTrackMapsH264NVENCToH264Mime(t *testing.T) {
	track, err := newTrack("video", "video", "h264_nvenc")
	if err != nil {
		t.Fatalf("newTrack returned error: %v", err)
	}

	if got, want := track.Codec().MimeType, pion.MimeTypeH264; got != want {
		t.Fatalf("track mime = %q, want %q", got, want)
	}
}

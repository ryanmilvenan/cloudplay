//go:build nvenc

package nvenc

import (
	"testing"
)

const (
	testWidth  = 320
	testHeight = 240
)

// yuvFrame generates a synthetic I420 frame of the given dimensions.
// All luma samples are set to y, chroma to 128 (neutral).
func yuvFrame(w, h int, y uint8) []byte {
	size := w*h + 2*(w>>1)*(h>>1)
	frame := make([]byte, size)
	// Y plane
	for i := 0; i < w*h; i++ {
		frame[i] = y
	}
	// U+V planes: neutral grey
	for i := w * h; i < size; i++ {
		frame[i] = 128
	}
	return frame
}

// TestNewEncoder verifies that NewEncoder opens successfully with defaults.
func TestNewEncoder(t *testing.T) {
	enc, err := NewEncoder(testWidth, testHeight, nil)
	if err != nil {
		t.Fatalf("NewEncoder failed: %v", err)
	}
	defer enc.Shutdown()

	if enc.ctx == nil {
		t.Fatal("expected non-nil encoder context")
	}
}

// TestNewEncoderWithOptions verifies custom preset/tune/bitrate.
func TestNewEncoderWithOptions(t *testing.T) {
	opts := &Options{
		Bitrate: 2000,
		Preset:  "p4",
		Tune:    "ll",
	}
	enc, err := NewEncoder(testWidth, testHeight, opts)
	if err != nil {
		t.Fatalf("NewEncoder with options failed: %v", err)
	}
	defer enc.Shutdown()
}

// TestEncode verifies that encoding a frame produces non-empty output.
func TestEncode(t *testing.T) {
	enc, err := NewEncoder(testWidth, testHeight, nil)
	if err != nil {
		t.Fatalf("NewEncoder failed: %v", err)
	}
	defer enc.Shutdown()

	frame := yuvFrame(testWidth, testHeight, 100)
	out := enc.Encode(frame)
	if len(out) == 0 {
		t.Fatal("Encode returned empty output for a valid frame")
	}
}

// TestEncodeMultipleFrames verifies that the encoder handles a sequence of frames.
func TestEncodeMultipleFrames(t *testing.T) {
	enc, err := NewEncoder(testWidth, testHeight, nil)
	if err != nil {
		t.Fatalf("NewEncoder failed: %v", err)
	}
	defer enc.Shutdown()

	encoded := 0
	for i := 0; i < 10; i++ {
		frame := yuvFrame(testWidth, testHeight, uint8(i*20))
		out := enc.Encode(frame)
		if len(out) > 0 {
			encoded++
		}
	}
	if encoded == 0 {
		t.Fatal("no frames were encoded in a 10-frame sequence")
	}
}

// TestInfo verifies that Info returns a non-empty string.
func TestInfo(t *testing.T) {
	enc, err := NewEncoder(testWidth, testHeight, nil)
	if err != nil {
		t.Fatalf("NewEncoder failed: %v", err)
	}
	defer enc.Shutdown()

	info := enc.Info()
	if info == "" {
		t.Fatal("Info returned empty string")
	}
	t.Logf("Encoder info: %s", info)
}

// TestIntraRefresh verifies that calling IntraRefresh does not panic and that
// the next encoded frame is still valid output.
func TestIntraRefresh(t *testing.T) {
	enc, err := NewEncoder(testWidth, testHeight, nil)
	if err != nil {
		t.Fatalf("NewEncoder failed: %v", err)
	}
	defer enc.Shutdown()

	// Seed the encoder with a couple of frames first
	for i := 0; i < 3; i++ {
		enc.Encode(yuvFrame(testWidth, testHeight, 80))
	}

	// Request intra refresh — must not panic
	enc.IntraRefresh()

	out := enc.Encode(yuvFrame(testWidth, testHeight, 80))
	if len(out) == 0 {
		t.Fatal("no output after IntraRefresh")
	}
}

// TestSetFlip verifies that SetFlip does not panic.
func TestSetFlip(t *testing.T) {
	enc, err := NewEncoder(testWidth, testHeight, nil)
	if err != nil {
		t.Fatalf("NewEncoder failed: %v", err)
	}
	defer enc.Shutdown()

	enc.SetFlip(true)
	enc.SetFlip(false)
}

// TestShutdown verifies that Shutdown is idempotent.
func TestShutdown(t *testing.T) {
	enc, err := NewEncoder(testWidth, testHeight, nil)
	if err != nil {
		t.Fatalf("NewEncoder failed: %v", err)
	}

	if err := enc.Shutdown(); err != nil {
		t.Fatalf("first Shutdown failed: %v", err)
	}
	if err := enc.Shutdown(); err != nil {
		t.Fatalf("second (idempotent) Shutdown failed: %v", err)
	}
}

// TestEncodeNilInput verifies that passing an empty slice does not panic.
func TestEncodeNilInput(t *testing.T) {
	enc, err := NewEncoder(testWidth, testHeight, nil)
	if err != nil {
		t.Fatalf("NewEncoder failed: %v", err)
	}
	defer enc.Shutdown()

	out := enc.Encode(nil)
	if out != nil {
		t.Fatalf("expected nil output for nil input, got %d bytes", len(out))
	}

	out = enc.Encode([]byte{})
	if out != nil {
		t.Fatalf("expected nil output for empty input, got %d bytes", len(out))
	}
}

// audio-canary: deterministic validator for the xemu PipeWire audio capture.
//
// Phase 4 lands the real implementation. Today this is a scaffold so
// `go build ./tools/...` works from Phase 0 onward.
//
// Intended behavior (Phase 4):
//   1. Spawn a 440 Hz sine generator that emits to the default PipeWire sink.
//   2. Use pkg/worker/caged/xemu/audiocap.go to capture ~5 s of audio.
//   3. FFT the captured samples, assert the peak bin is 440 Hz (±2 Hz, -3 dB).
//   4. Exit 0 on success, non-zero with diagnostics otherwise.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "audio-canary: not yet implemented (Phase 4 deliverable). "+
		"See tools/audio-canary/README.md.")
	os.Exit(2)
}

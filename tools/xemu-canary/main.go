// xemu-canary: deterministic validator for the xemu LD_PRELOAD GL-capture shim.
//
// Phase 3 will land the real implementation. Today this is a scaffold so
// `go build ./tools/...` works from Phase 0 onward.
//
// Intended behavior (Phase 3):
//   1. Build tools/videocap_preload (from pkg/worker/caged/xemu/videocap_preload.c).
//   2. Spawn a small GL demo rendering one of three fixed RGBA patterns.
//   3. LD_PRELOAD the shim into the demo, capture 10 frames per pattern.
//   4. SHA256 each frame, compare to testdata/frame_<pattern>_<n>.sha256.
//   5. Exit 0 on all-match, non-zero otherwise with a clear diff.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "xemu-canary: not yet implemented (Phase 3 deliverable). "+
		"See tools/xemu-canary/README.md.")
	os.Exit(2)
}

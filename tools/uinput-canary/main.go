// uinput-canary: deterministic validator for the xemu uinput virtual-gamepad
// input injection path.
//
// Phase 5 lands the real implementation. Today this is a scaffold so
// `go build ./tools/...` works from Phase 0 onward.
//
// Intended behavior (Phase 5):
//   1. Use pkg/worker/caged/xemu/input.go to create a virtual Xbox gamepad.
//   2. Spawn `evtest` (or an equivalent reader) targeting the new device.
//   3. Replay a scripted input sequence (A/B/X/Y/LS/RS/LT/RT/D-pad/sticks).
//   4. Diff captured evdev events against testdata/golden_events.txt.
//   5. Exit 0 iff empty diff.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "uinput-canary: not yet implemented (Phase 5 deliverable). "+
		"See tools/uinput-canary/README.md.")
	os.Exit(2)
}

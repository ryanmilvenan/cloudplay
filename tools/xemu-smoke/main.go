// xemu-smoke: process-lifecycle smoke test for the xemu native backend.
//
// Phase 2 lands the real implementation. Today this is a scaffold so
// `go build ./tools/...` works from Phase 0 onward.
//
// Intended behavior (Phase 2):
//   1. Use pkg/worker/caged/xemu/xvfb.go to allocate a display.
//   2. Use pkg/worker/caged/xemu/process.go to start xemu (BIOS boot, no ROM).
//   3. Sleep 5 s; SIGTERM the process.
//   4. Assert clean shutdown: exit within 2 s, no zombie xemu/Xvfb in pgrep.
//   5. Repeat 10 times. Exit 0 iff every iteration is clean.
//
// Phase 3 extension: after boot, SHA256 the first captured frame and diff
// against testdata/dashboard_boot_frame_0.sha256 (requires Xbox MCPX+flash
// BIOS files bind-mounted into /xemu-bios/).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "xemu-smoke: not yet implemented (Phase 2 deliverable). "+
		"See tools/xemu-smoke/README.md.")
	os.Exit(2)
}

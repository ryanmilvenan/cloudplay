// Package rcheevos wraps the RetroAchievements rcheevos C library
// (https://github.com/RetroAchievements/rcheevos, vendored under
// upstream/) so the worker can hash a loaded ROM, fetch the
// achievement set for that game from retroachievements.org, evaluate
// achievement triggers against emulator RAM every frame, and post
// unlocks on behalf of the host user.
//
// Phase 1 of this package just exposes the rcheevos version string —
// this is a pipeline check: we verify the vendored C sources compile
// into librcheevos.a (built at Docker build time — see Dockerfile),
// that CGo linkage works, and that the worker binary still boots with
// the library included. Real achievement wiring lands in subsequent
// commits.

package rcheevos

// #cgo CFLAGS: -I${SRCDIR}/upstream/include -I${SRCDIR}/upstream/src
// #cgo LDFLAGS: -L${SRCDIR}/upstream/build -lrcheevos
// #include "rc_version.h"
import "C"

// Version returns the rcheevos library version in "major.minor.patch"
// form. Useful for startup logs and verifying the linked library is
// actually present.
func Version() string { return C.GoString(C.rc_version_string()) }

//go:build !(nvenc && linux)

package nvenc

import "fmt"

// ExtMemHandle is the stub type on non-Linux or non-NVENC builds.
// Its opaque zero value is safe to pass to ReleaseExternalMemory.
type ExtMemHandle struct{}

// ImportExternalMemory is a stub on non-Linux or non-NVENC builds.
func ImportExternalMemory(_ int, _ uint64) (uintptr, *ExtMemHandle, error) {
	return 0, nil, fmt.Errorf("nvenc: ImportExternalMemory requires -tags 'nvenc linux'")
}

// ReleaseExternalMemory is a stub on non-Linux or non-NVENC builds.
func ReleaseExternalMemory(_ *ExtMemHandle) {}

// EncodeFromDevPtr is a stub on non-Linux or non-NVENC builds.
func (e *NVENC) EncodeFromDevPtr(_ uintptr, _ uint64) ([]byte, error) {
	return nil, fmt.Errorf("nvenc: EncodeFromDevPtr requires -tags 'nvenc linux'")
}

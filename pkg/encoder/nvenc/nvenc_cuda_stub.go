//go:build !(nvenc && linux)

package nvenc

import "fmt"

// ImportExternalMemory is a stub on non-Linux or non-NVENC builds.
func ImportExternalMemory(_ int, _ uint64) (uintptr, error) {
	return 0, fmt.Errorf("nvenc: ImportExternalMemory requires -tags 'nvenc linux'")
}

// EncodeFromDevPtr is a stub on non-Linux or non-NVENC builds.
func (e *NVENC) EncodeFromDevPtr(_ uintptr, _ uint64) ([]byte, error) {
	return nil, fmt.Errorf("nvenc: EncodeFromDevPtr requires -tags 'nvenc linux'")
}

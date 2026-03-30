//go:build !nvenc

// Package nvenc provides an h264_nvenc hardware encoder via FFmpeg + CUDA.
// Build with the `nvenc` tag to enable: go build -tags nvenc ./...
//
// Without the nvenc tag, this package exports only stub types so that
// encoder.go can reference nvenc.Options in the config struct without
// requiring NVENC headers on every build host.
package nvenc

import "fmt"

// Options holds NVENC encoder configuration.
// Fields are the same whether or not the nvenc build tag is active.
type Options struct {
	Bitrate int
	Preset  string
	Tune    string
}

// NVENC is a placeholder type when the nvenc build tag is not present.
type NVENC struct{}

// NewEncoder always returns an error on non-NVENC builds.
func NewEncoder(w, h int, opts *Options) (*NVENC, error) {
	return nil, fmt.Errorf("nvenc: encoder not available (rebuild with -tags nvenc)")
}

func (e *NVENC) Encode([]byte) []byte  { return nil }
func (e *NVENC) IntraRefresh()         {}
func (e *NVENC) Info() string          { return "nvenc (disabled)" }
func (e *NVENC) SetFlip(bool)          {}
func (e *NVENC) Shutdown() error       { return nil }

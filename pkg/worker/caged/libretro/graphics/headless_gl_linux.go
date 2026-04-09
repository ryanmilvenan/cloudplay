//go:build linux

package graphics

import "fmt"

func newRequestedHeadlessGLContext(cfg Config, backend string) (HeadlessGLContext, error) {
	switch backend {
	case "egl":
		return NewEGLContext(cfg)
	default:
		return nil, fmt.Errorf("graphics: unsupported GL backend %q", backend)
	}
}

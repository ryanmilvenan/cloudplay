//go:build !linux

package graphics

import "fmt"

func newRequestedHeadlessGLContext(cfg Config, backend string) (HeadlessGLContext, error) {
	return nil, fmt.Errorf("graphics: GL backend %q is only supported on linux", backend)
}

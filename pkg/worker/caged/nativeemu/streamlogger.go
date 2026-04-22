// Package nativeemu provides the shared scaffolding for backends that drive
// a standalone OS-process emulator (xemu, flycast, etc.) and expose it
// through the worker's app.App surface. Each concrete backend composes these
// primitives: a virtual X display, an ffmpeg-based frame capture, a private
// PipeWire/Pulse session for audio routing, a parec-based capture pipeline,
// a uinput virtual gamepad, and a generic process supervisor.
package nativeemu

import (
	"bytes"
	"io"
	"strings"
	"sync"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// streamLogger adapts a zerolog-style Logger to the io.Writer interface
// exec.Cmd expects for Stdout/Stderr. It buffers by line so multi-line
// subprocess output lands in the log as one-line-per-log-event rather than
// arbitrary byte-count chunks.
type streamLogger struct {
	log    *logger.Logger
	prefix string
	mu     sync.Mutex
	buf    bytes.Buffer
}

// newStreamLogger returns an io.Writer that tags every captured line with
// prefix and forwards it to log at Info level.
func newStreamLogger(log *logger.Logger, prefix string) io.Writer {
	return &streamLogger{log: log, prefix: prefix}
}

func (s *streamLogger) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, _ := s.buf.Write(p)
	for {
		line, err := s.buf.ReadString('\n')
		if err != nil {
			// Partial — put it back and wait for more bytes to close it out.
			s.buf.Reset()
			s.buf.WriteString(line)
			return n, nil
		}
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			s.log.Info().Msgf("%s%s", s.prefix, line)
		}
	}
}

var _ io.Writer = (*streamLogger)(nil)

package xemu

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
)

// videocapMagic must match VIDEOCAP_MAGIC in videocap_preload.c.
// The value is the byte sequence 'F','M','R','V' read as a little-endian
// uint32 — the letters are a nod to a mnemonic in dev notes, nothing more.
const videocapMagic uint32 = 0x56524d46

// videocapHeaderSize: seven uint32 fields packed little-endian.
const videocapHeaderSize = 7 * 4

// Videocap is the receiver half of the video-capture pipeline. It listens
// on a Unix socket, accepts one connection from the LD_PRELOAD shim, and
// routes each framed RGBA payload to the configured callback.
//
// Lifecycle:
//
//	v := Videocap{Log: log}
//	if err := v.Start(onVideo); err != nil { ... }
//	// (xemu spawns; shim connects; frames flow to onVideo)
//	v.Close()
//
// SocketPath returns the absolute path the shim should connect to; callers
// pass it to xemu via the CLOUDPLAY_VIDEOCAP_SOCKET env var.
type Videocap struct {
	// Log receives info/warn messages; required.
	Log *logger.Logger
	// Dir is the parent directory for the socket file. Defaults to os.TempDir.
	Dir string

	sockPath string
	listener *net.UnixListener
	cb       func(app.Video)

	mu         sync.Mutex
	started    bool
	closing    atomic.Bool
	doneCh     chan struct{}
	framesRecv atomic.Uint64
	lastConn   atomic.Pointer[net.UnixConn]
}

// Start binds a Unix socket and begins accepting the shim's connection.
// The callback fires from an internal goroutine on each frame received.
// It returns once the listener is bound (before any frame arrives).
func (v *Videocap) Start(cb func(app.Video)) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.started {
		return errors.New("videocap: already started")
	}
	if cb == nil {
		return errors.New("videocap: callback is required")
	}
	dir := v.Dir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("videocap: mkdir %s: %w", dir, err)
	}
	// Unique per-process socket path so overlapping sessions don't collide.
	v.sockPath = filepath.Join(dir,
		fmt.Sprintf("xemu-videocap-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	_ = os.Remove(v.sockPath)

	addr := &net.UnixAddr{Name: v.sockPath, Net: "unix"}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("videocap: listen %s: %w", v.sockPath, err)
	}
	v.listener = l
	v.cb = cb
	v.started = true
	v.doneCh = make(chan struct{})
	go v.acceptLoop()
	v.Log.Info().Str("sock", v.sockPath).Msg("[XEMU-VIDEO] receiver ready")
	return nil
}

// SocketPath returns the absolute path of the Unix socket. Safe to call
// any time after Start. Empty before Start.
func (v *Videocap) SocketPath() string { return v.sockPath }

// FramesReceived reports the cumulative number of frames the shim has
// delivered. Useful for tests and diagnostics.
func (v *Videocap) FramesReceived() uint64 { return v.framesRecv.Load() }

// Close tears down the listener, the connection, and the goroutine.
// Safe to call multiple times, safe to call before Start has returned.
func (v *Videocap) Close() error {
	v.mu.Lock()
	if !v.started {
		v.mu.Unlock()
		return nil
	}
	if !v.closing.CompareAndSwap(false, true) {
		v.mu.Unlock()
		return nil
	}
	if v.listener != nil {
		_ = v.listener.Close()
	}
	if c := v.lastConn.Load(); c != nil {
		_ = c.Close()
	}
	done := v.doneCh
	v.mu.Unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		v.Log.Warn().Msg("[XEMU-VIDEO] close timeout after 2s")
	}
	_ = os.Remove(v.sockPath)
	v.Log.Info().Uint64("frames", v.framesRecv.Load()).Msg("[XEMU-VIDEO] receiver closed")
	return nil
}

func (v *Videocap) acceptLoop() {
	defer close(v.doneCh)
	// The shim only opens one connection per process lifetime. Accept once.
	conn, err := v.listener.AcceptUnix()
	if err != nil {
		if !v.closing.Load() {
			v.Log.Warn().Err(err).Msg("[XEMU-VIDEO] accept failed")
		}
		return
	}
	v.lastConn.Store(conn)
	v.Log.Info().Msg("[XEMU-VIDEO] shim connected")

	if err := v.readFrames(conn); err != nil && !v.closing.Load() {
		v.Log.Warn().Err(err).Msg("[XEMU-VIDEO] read loop ended with error")
	}
	_ = conn.Close()
}

// readFrames consumes frames from the shim until EOF or error. Each frame
// allocates a fresh []byte so downstream consumers (encoder, recorder) can
// retain the slice safely.
func (v *Videocap) readFrames(conn *net.UnixConn) error {
	hdr := make([]byte, videocapHeaderSize)
	lastLog := time.Now()
	sinceLog := 0
	for {
		if v.closing.Load() {
			return nil
		}
		if _, err := io.ReadFull(conn, hdr); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		magic := binary.LittleEndian.Uint32(hdr[0:4])
		if magic != videocapMagic {
			return fmt.Errorf("videocap: bad magic 0x%x", magic)
		}
		w := int(binary.LittleEndian.Uint32(hdr[4:8]))
		h := int(binary.LittleEndian.Uint32(hdr[8:12]))
		stride := int(binary.LittleEndian.Uint32(hdr[12:16]))
		_ = binary.LittleEndian.Uint32(hdr[16:20]) // format; only RGBA8 today
		_ = binary.LittleEndian.Uint32(hdr[20:24]) // seq; diagnostics only
		length := int(binary.LittleEndian.Uint32(hdr[24:28]))

		if w <= 0 || h <= 0 || w > 4096 || h > 4096 {
			return fmt.Errorf("videocap: impossible dimensions %dx%d", w, h)
		}
		if length != w*h*4 {
			return fmt.Errorf("videocap: length mismatch %d != %d", length, w*h*4)
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return fmt.Errorf("videocap: read payload: %w", err)
		}
		v.framesRecv.Add(1)
		sinceLog++
		if time.Since(lastLog) >= 5*time.Second {
			v.Log.Info().Int("w", w).Int("h", h).Int("recent_fps", sinceLog/5).
				Uint64("total", v.framesRecv.Load()).Msg("[XEMU-VIDEO] frame flow")
			lastLog = time.Now()
			sinceLog = 0
		}

		// glReadPixels hands us bottom-up frames; downstream expects
		// top-down, so flip in-place. If this shows up on a profile,
		// move the flip into the shim so the 1.2 MB/frame memcpy
		// happens once, outside the Go GC path.
		flipVertical(buf, h, stride)

		v.cb(app.Video{
			Frame:    app.RawFrame{Data: buf, Stride: stride, W: w, H: h},
			Duration: 0,
		})
	}
}

// flipVertical flips an image top↔bottom in-place. `stride` is bytes per row.
func flipVertical(buf []byte, h, stride int) {
	tmp := make([]byte, stride)
	for y := 0; y < h/2; y++ {
		top := y * stride
		bot := (h - 1 - y) * stride
		copy(tmp, buf[top:top+stride])
		copy(buf[top:top+stride], buf[bot:bot+stride])
		copy(buf[bot:bot+stride], tmp)
	}
}

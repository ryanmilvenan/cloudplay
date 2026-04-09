package room

import (
	"iter"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
)

type MediaPipe interface {
	// Destroy frees all allocated resources.
	Destroy()
	// Init initializes the pipe: allocates needed resources.
	Init() error
	// Reinit initializes video and audio pipes with the new settings.
	Reinit() error
	// PushAudio pushes the 16bit PCM audio frames into an encoder.
	// Because we need to fill the buffer, the SetAudioCb should be
	// used in order to get the result.
	PushAudio([]int16)
	// ProcessVideo returns encoded video frame.
	ProcessVideo(app.Video) []byte
	// SetAudioCb sets a callback for encoded audio data with its frame duration (ns).
	SetAudioCb(func(data []byte, duration int32))
}

type SessionManager[T Session] interface {
	Add(T) bool
	Empty() bool
	Find(string) T
	RemoveL(T) int
	// Reset used for proper cleanup of the resources if needed.
	Reset()
	Values() iter.Seq[T]
}

type Session interface {
	Disconnect()
	SendAudio([]byte, int32)
	SendVideo([]byte, int32)
	SendData([]byte)
}

type SessionKey string

func (s SessionKey) String() string { return string(s) }
func (s SessionKey) Id() string     { return s.String() }

type Room[T Session] struct {
	app   app.App
	id    string
	media MediaPipe
	users SessionManager[T]

	pacer       *framePacer
	closed      bool
	HandleClose func()
}

func NewRoom[T Session](id string, app app.App, um SessionManager[T], media MediaPipe) *Room[T] {
	room := &Room[T]{id: id, app: app, users: um, media: media}
	if app != nil && media != nil {
		room.InitVideo()
		room.InitAudio()
	}
	return room
}

func (r *Room[T]) InitAudio() {
	r.app.SetAudioCb(func(a app.Audio) { r.media.PushAudio(a.Data) })
	r.media.SetAudioCb(func(d []byte, l int32) {
		for u := range r.users.Values() {
			u.SendAudio(d, l)
		}
	})
}

// framePacer decouples encoding from sending.
// The emulator goroutine stores raw video frames (unencoded).
// A separate ticker goroutine wakes at fixed intervals, encodes the
// latest frame, and sends it to all connected users — ensuring even
// cadence regardless of emulator timing variance.
//
// Key design: encoding happens on the ticker goroutine, NOT the emulator
// callback. This prevents the synchronous GPU encode from blocking
// the emulator loop.
//
// For the zero-copy path this works because ProcessVideoZeroCopy ignores
// the app.Video data entirely — it reads from the CUDA devPtr which was
// populated by BlitFrom in go_wait_sync_index (between frames, queue idle).
// The GPU buffer remains stable until the next BlitFrom call.
type framePacer struct {
	mu        sync.Mutex
	latestVid *app.Video // latest raw video from emulator (replaced each frame)
	lastData  []byte     // previously sent encoded frame (for dup on underrun)
	dur       int64      // accumulated duration in nanoseconds
	tickNs    int64      // target interval in nanoseconds
	stopped   int32      // atomic flag
	diagSent  int64      // counter for diagnostic logging
}

func newFramePacer(targetFPS int) *framePacer {
	if targetFPS <= 0 {
		targetFPS = 30
	}
	return &framePacer{
		tickNs: int64(time.Second) / int64(targetFPS),
	}
}

// submitRaw stores the latest raw video frame. Called from the emulator goroutine.
// Deep-copies Frame.Data because the emulator reuses the buffer between frames.
// For the zero-copy path Frame.Data may be nil/empty — that's fine, the encode
// path reads from the GPU devPtr instead.
func (fp *framePacer) submitRaw(v app.Video) {
	cp := v
	if len(v.Frame.Data) > 0 {
		buf := make([]byte, len(v.Frame.Data))
		copy(buf, v.Frame.Data)
		cp.Frame.Data = buf
	}
	fp.mu.Lock()
	fp.latestVid = &cp
	fp.dur += int64(v.Duration)
	fp.mu.Unlock()
}

// tick is called by the pacer goroutine at fixed intervals.
// It takes the latest raw frame, encodes it, and returns encoded bytes + duration.
func (fp *framePacer) tick(encode func(app.Video) []byte) ([]byte, int64) {
	fp.mu.Lock()
	vid := fp.latestVid
	fp.latestVid = nil
	dur := fp.dur
	if dur < fp.tickNs {
		dur = fp.tickNs
	}
	fp.dur = 0
	fp.mu.Unlock()

	var data []byte
	if vid != nil {
		data = encode(*vid)
	}

	if data != nil {
		fp.mu.Lock()
		fp.lastData = data
		fp.mu.Unlock()
	} else {
		// Underrun or encode returned empty: dup last frame
		fp.mu.Lock()
		data = fp.lastData
		fp.mu.Unlock()
		dur = fp.tickNs
	}

	if data != nil {
		n := atomic.AddInt64(&fp.diagSent, 1)
		if n <= 3 || n%300 == 0 {
			kind := "fresh"
			if vid == nil {
				kind = "dup"
			}
			log.Printf("[cloudplay pacer] tick=%d len=%d dur=%d type=%s", n, len(data), dur, kind)
		}
	}
	return data, dur
}

func (fp *framePacer) stop() {
	atomic.StoreInt32(&fp.stopped, 1)
}

func (r *Room[T]) InitVideo() {
	r.pacer = newFramePacer(30)
	pacer := r.pacer

	// Emulator callback: just stash the raw frame, no encoding.
	// Encoding is deferred to the ticker goroutine to avoid blocking
	// the emulator loop with synchronous GPU work.
	r.app.SetVideoCb(func(v app.Video) {
		pacer.submitRaw(v)
	})

	// Pacer goroutine: wake at fixed 30fps intervals, encode latest frame, send.
	go func() {
		ticker := time.NewTicker(time.Duration(pacer.tickNs))
		defer ticker.Stop()
		for range ticker.C {
			if atomic.LoadInt32(&pacer.stopped) != 0 {
				return
			}
			data, dur := pacer.tick(r.media.ProcessVideo)
			if data == nil {
				continue
			}
			for u := range r.users.Values() {
				u.SendVideo(data, int32(dur))
			}
		}
	}()
}

func (r *Room[T]) App() app.App         { return r.app }
func (r *Room[T]) BindAppMedia()        { r.InitAudio(); r.InitVideo() }
func (r *Room[T]) Id() string           { return r.id }
func (r *Room[T]) SetApp(app app.App)   { r.app = app }
func (r *Room[T]) SetMedia(m MediaPipe) { r.media = m }
func (r *Room[T]) StartApp()            { r.app.Start() }
func (r *Room[T]) Send(data []byte) {
	for u := range r.users.Values() {
		u.SendData(data)
	}
}

func (r *Room[T]) Close() {
	if r == nil || r.closed {
		return
	}
	r.closed = true

	if r.pacer != nil {
		r.pacer.stop()
	}
	if r.app != nil {
		r.app.Close()
	}
	if r.media != nil {
		r.media.Destroy()
	}
	if r.HandleClose != nil {
		r.HandleClose()
	}
}

// Router tracks and routes freshly connected users to an app room.
// Rooms and users has 1-to-n relationship.
type Router[T Session] struct {
	room  *Room[T]
	users SessionManager[T]
	mu    sync.Mutex
}

func (r *Router[T]) FindRoom(id string) *Room[T] {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.room != nil && r.room.Id() == id {
		return r.room
	}
	return nil
}

func (r *Router[T]) Remove(user T) {
	if left := r.users.RemoveL(user); left == 0 {
		r.Close()
		r.SetRoom(nil) // !to remove
	}
}

func (r *Router[T]) AddUser(user T)           { r.users.Add(user) }
func (r *Router[T]) Close()                   { r.mu.Lock(); r.room.Close(); r.room = nil; r.mu.Unlock() }
func (r *Router[T]) FindUser(uid string) T    { return r.users.Find(uid) }
func (r *Router[T]) Room() *Room[T]           { r.mu.Lock(); defer r.mu.Unlock(); return r.room }
func (r *Router[T]) SetRoom(room *Room[T])    { r.mu.Lock(); r.room = room; r.mu.Unlock() }
func (r *Router[T]) HasRoom() bool            { r.mu.Lock(); defer r.mu.Unlock(); return r.room != nil }
func (r *Router[T]) Users() SessionManager[T] { return r.users }
func (r *Router[T]) Reset() {
	r.mu.Lock()
	if r.room != nil {
		r.room.Close()
		r.room = nil
	}
	for u := range r.users.Values() {
		u.Disconnect()
	}
	r.users.Reset()
	r.mu.Unlock()
}

type AppSession struct {
	Session
	uid SessionKey
}

func (p AppSession) Id() SessionKey { return p.uid }

type GameSession struct {
	AppSession
	Index int // track user Index (i.e. player 1,2,3,4 select)
}

func NewGameSession(id string, s Session) *GameSession {
	return &GameSession{AppSession: AppSession{uid: SessionKey(id), Session: s}}
}

// Package rcheevos wraps the RetroAchievements rc_client C library
// so the worker can: hash a loaded ROM, fetch its achievement set,
// evaluate triggers against emulator RAM each frame, and post unlocks
// on behalf of the host user.
//
// Current scope: create the client, log in via API token, destroy.
// Memory access is stubbed (returns zeros) until a subsequent commit
// wires it to libretro's retro_get_memory_data. HTTP is real — login
// actually reaches retroachievements.org.

package rcheevos

/*
#cgo CFLAGS: -I${SRCDIR}/upstream/include -I${SRCDIR}/upstream/src
#cgo LDFLAGS: -L${SRCDIR}/upstream/build -lrcheevos

#include <stdlib.h>
#include <rc_client.h>
#include <rc_version.h>

rc_client_t* rcheevos_create(void);
void         rcheevos_begin_login(rc_client_t* client, const char* username, const char* token, uintptr_t userdata);
void         rcheevos_begin_login_password(rc_client_t* client, const char* username, const char* password, uintptr_t userdata);
int          rcheevos_hash_file(uint32_t console_id, const char* path, char* out_hash);
void         rcheevos_begin_load_game(rc_client_t* client, const char* hash, uintptr_t userdata);

// rc_client_do_frame wrapper so Go can call it through the typed handle.
static void rcheevos_do_frame(rc_client_t* client) { rc_client_do_frame(client); }

// rcheevos_game_info returns the title + count of achievements in the
// currently loaded game, or NULLs / zero if no game is loaded.
static const char* rcheevos_game_title(rc_client_t* client) {
    const rc_client_game_t* g = rc_client_get_game_info(client);
    return g ? g->title : NULL;
}
static uint32_t rcheevos_achievement_count(rc_client_t* client) {
    rc_client_achievement_list_t* list = rc_client_create_achievement_list(
        client, RC_CLIENT_ACHIEVEMENT_CATEGORY_CORE, RC_CLIENT_ACHIEVEMENT_LIST_GROUPING_LOCK_STATE);
    if (!list) return 0;
    uint32_t count = 0;
    for (uint32_t i = 0; i < list->num_buckets; i++) count += list->buckets[i].num_achievements;
    rc_client_destroy_achievement_list(list);
    return count;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime/cgo"
	"sync"
	"unsafe"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// Version returns the rcheevos library version (e.g. "12.3").
func Version() string { return C.GoString(C.rc_version_string()) }

// Unlock is handed to the AchievementHandler when rc_client reports
// an achievement has been earned (RC_CLIENT_EVENT_ACHIEVEMENT_TRIGGERED).
type Unlock struct {
	ID          uint32
	Title       string
	Description string
	Points      uint32
	BadgeURL    string
}

// AchievementHandler is invoked on the emulator thread right after
// an unlock fires. Keep it lightweight — push to a channel / spawn a
// goroutine if you need to do blocking work.
type AchievementHandler func(Unlock)

// MemoryReader reads numBytes starting at address from emulator RAM
// into dst and returns how many bytes were actually readable.
// Returning 0 for any given address tells rc_client the region is
// currently unavailable — it'll retry on the next frame.
type MemoryReader func(address uint32, dst []byte) uint32

// Client wraps an rc_client_t with Go-friendly sync for async calls.
type Client struct {
	handle *C.rc_client_t
	log    *logger.Logger

	loginMu  sync.Mutex
	loginErr error
	loginC   chan struct{}
	loginH   cgo.Handle

	loadMu  sync.Mutex
	loadErr error
	loadC   chan struct{}
	loadH   cgo.Handle

	memMu   sync.RWMutex
	memRead MemoryReader

	achMu      sync.RWMutex
	onUnlocked AchievementHandler
}

// NewClient creates an rc_client. Returns an error if rc_client_create
// fails (returns NULL — typically OOM).
func NewClient(log *logger.Logger) (*Client, error) {
	h := C.rcheevos_create()
	if h == nil {
		return nil, errors.New("rc_client_create returned NULL")
	}
	c := &Client{handle: h, log: log}
	pin(h, c)
	return c, nil
}

// Close releases the client and its registration in the handle table.
func (c *Client) Close() {
	if c == nil || c.handle == nil {
		return
	}
	unpin(c.handle)
	C.rc_client_destroy(c.handle)
	c.handle = nil
}

// Login authenticates against retroachievements.org. Prefers
// password-based login (which works with what users see in their RA
// control panel / know as their password). Blocks until the server
// responds. On success rc_client internally caches a session token
// for subsequent game loads / unlock posts.
func (c *Client) Login(username, secret string) error {
	return c.login(username, secret, false)
}

// LoginWithToken uses a cached Connect session token (not the Web API
// key from the control panel — see api-docs.retroachievements.org).
func (c *Client) LoginWithToken(username, token string) error {
	return c.login(username, token, true)
}

func (c *Client) login(username, secret string, isToken bool) error {
	if c == nil || c.handle == nil {
		return errors.New("rcheevos client not initialised")
	}

	c.loginMu.Lock()
	c.loginErr = nil
	c.loginC = make(chan struct{})
	c.loginH = cgo.NewHandle(c)
	c.loginMu.Unlock()
	defer func() {
		c.loginMu.Lock()
		c.loginH.Delete()
		c.loginH = 0
		c.loginMu.Unlock()
	}()

	cu := C.CString(username)
	cs := C.CString(secret)
	defer C.free(unsafe.Pointer(cu))
	defer C.free(unsafe.Pointer(cs))

	if isToken {
		C.rcheevos_begin_login(c.handle, cu, cs, C.uintptr_t(c.loginH))
	} else {
		C.rcheevos_begin_login_password(c.handle, cu, cs, C.uintptr_t(c.loginH))
	}

	<-c.loginC
	return c.loginErr
}

// SetAchievementHandler installs a function to call on every
// achievement unlock. Pass nil to detach.
func (c *Client) SetAchievementHandler(h AchievementHandler) {
	c.achMu.Lock()
	c.onUnlocked = h
	c.achMu.Unlock()
}

// dispatchUnlock is called from the exported event bridge.
func (c *Client) dispatchUnlock(u Unlock) {
	c.achMu.RLock()
	h := c.onUnlocked
	c.achMu.RUnlock()
	if h != nil {
		h(u)
	}
}

// SetMemoryReader installs a reader that rc_client will call to
// fetch emulator RAM bytes during rc_client_do_frame. Pass nil to
// detach (reverts to the default zero-fill stub).
func (c *Client) SetMemoryReader(r MemoryReader) {
	c.memMu.Lock()
	c.memRead = r
	c.memMu.Unlock()
}

// readMemory is invoked from the read-memory C bridge. Thread-safe
// read of the current reader.
func (c *Client) readMemory(address uint32, dst []byte) uint32 {
	c.memMu.RLock()
	r := c.memRead
	c.memMu.RUnlock()
	if r == nil {
		return 0
	}
	return r(address, dst)
}

// ConsoleID maps a cloudplay system identifier (as used in
// config.yaml — 'snes', 'n64', 'ps2' etc.) to the RC_CONSOLE_* enum
// rcheevos uses for ROM hashing and achievement-set lookup.
// Unknown systems map to 0 (RC_CONSOLE_UNKNOWN), which means the
// load-game flow will fail with "unsupported system" — not a crash.
func ConsoleID(system string) uint32 {
	switch system {
	case "snes":
		return 3
	case "n64":
		return 2
	case "nes":
		return 7
	case "gba":
		return 5
	case "gbc":
		return 6
	case "gb":
		return 4
	case "genesis", "megadrive":
		return 1
	case "ps1", "pcsx":
		return 12
	case "ps2":
		return 21
	case "gc":
		return 16
	case "wii":
		return 19
	case "dreamcast":
		return 40
	case "dos":
		return 26
	case "mame":
		return 27
	}
	return 0
}

// LoadGameFromFile computes the RA hash for the ROM at path (based on
// the given cloudplay system string), then asks rc_client to fetch
// the achievement set. Blocks until the server responds.
func (c *Client) LoadGameFromFile(path, system string) error {
	if c == nil || c.handle == nil {
		return errors.New("rcheevos client not initialised")
	}
	consoleID := ConsoleID(system)
	if consoleID == 0 {
		return fmt.Errorf("unknown rcheevos console for system %q", system)
	}

	var hash [33]C.char
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	if rc := C.rcheevos_hash_file(C.uint32_t(consoleID), cpath, &hash[0]); rc != 0 {
		return fmt.Errorf("rc_hash_generate_from_file returned %d", int(rc))
	}

	c.loadMu.Lock()
	c.loadErr = nil
	c.loadC = make(chan struct{})
	c.loadH = cgo.NewHandle(c)
	c.loadMu.Unlock()
	defer func() {
		c.loadMu.Lock()
		c.loadH.Delete()
		c.loadH = 0
		c.loadMu.Unlock()
	}()

	C.rcheevos_begin_load_game(c.handle, &hash[0], C.uintptr_t(c.loadH))

	<-c.loadC
	return c.loadErr
}

// finishLoadGame is invoked by the exported load-game completion bridge.
func (c *Client) finishLoadGame(result C.int, errorMessage *C.char) {
	if result != 0 {
		msg := C.GoString(errorMessage)
		if msg == "" {
			msg = fmt.Sprintf("rc_client load_game failed with code %d", int(result))
		}
		c.loadErr = errors.New(msg)
	}
	close(c.loadC)
}

// GameTitle returns the title of the loaded game, or "" if none.
func (c *Client) GameTitle() string {
	if c == nil || c.handle == nil {
		return ""
	}
	title := C.rcheevos_game_title(c.handle)
	if title == nil {
		return ""
	}
	return C.GoString(title)
}

// AchievementCount returns how many core achievements the current
// game has. Zero when no game is loaded.
func (c *Client) AchievementCount() int {
	if c == nil || c.handle == nil {
		return 0
	}
	return int(C.rcheevos_achievement_count(c.handle))
}

// DoFrame runs one rcheevos evaluation tick. Call from the emulator
// thread after each retro_run so the read-memory bridge sees RAM in
// a valid state. Cheap when no game is loaded.
func (c *Client) DoFrame() {
	if c == nil || c.handle == nil {
		return
	}
	C.rcheevos_do_frame(c.handle)
}

// User returns the logged-in user's display name. Empty if not logged in.
func (c *Client) User() string {
	if c == nil || c.handle == nil {
		return ""
	}
	u := C.rc_client_get_user_info(c.handle)
	if u == nil {
		return ""
	}
	return C.GoString(u.display_name)
}

// finishLogin is called by the exported completion bridge.
func (c *Client) finishLogin(result C.int, errorMessage *C.char) {
	if result != 0 {
		msg := C.GoString(errorMessage)
		if msg == "" {
			msg = fmt.Sprintf("rc_client login failed with code %d", int(result))
		}
		c.loginErr = errors.New(msg)
	}
	close(c.loginC)
}

// ---- handle table: rc_client_t* → *Client so bridge callbacks can
// route back into the right Go instance ----

var (
	clientsMu sync.RWMutex
	clients   = map[*C.rc_client_t]*Client{}
)

func pin(h *C.rc_client_t, c *Client)         { clientsMu.Lock(); clients[h] = c; clientsMu.Unlock() }
func unpin(h *C.rc_client_t)                  { clientsMu.Lock(); delete(clients, h); clientsMu.Unlock() }
func clientByHandle(h *C.rc_client_t) *Client { clientsMu.RLock(); defer clientsMu.RUnlock(); return clients[h] }

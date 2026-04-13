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

// Client wraps an rc_client_t with Go-friendly sync for async calls.
type Client struct {
	handle *C.rc_client_t
	log    *logger.Logger

	loginMu  sync.Mutex
	loginErr error
	loginC   chan struct{}
	loginH   cgo.Handle
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

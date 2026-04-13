// CGo bridges: Go-exported functions that bridge.c references via
// `extern` declarations. rc_client receives these as C function
// pointers (through rcheevos_create in bridge.c).

package rcheevos

/*
#include <stdlib.h>
#include <string.h>
#include <rc_client.h>

void rcheevos_invoke_server_callback(rc_client_server_callback_t cb, void* callback_data,
                                     const char* body, size_t body_length, int http_status_code);
*/
import "C"

import (
	"bytes"
	"io"
	"net/http"
	"runtime/cgo"
	"time"
	"unsafe"
)

//export rcheevos_read_memory_bridge
func rcheevos_read_memory_bridge(address C.uint32_t, buffer *C.uint8_t, numBytes C.uint32_t, client *C.rc_client_t) C.uint32_t {
	if buffer == nil || numBytes == 0 {
		return 0
	}
	c := clientByHandle(client)
	if c == nil {
		return 0
	}
	dst := unsafe.Slice((*byte)(unsafe.Pointer(buffer)), int(numBytes))
	n := c.readMemory(uint32(address), dst)
	if n < numBytes {
		// Zero the remainder so rc_client doesn't see stale garbage.
		for i := int(n); i < int(numBytes); i++ {
			dst[i] = 0
		}
	}
	return C.uint32_t(n)
}

//export rcheevos_log_bridge
func rcheevos_log_bridge(message *C.char, client *C.rc_client_t) {
	c := clientByHandle(client)
	if c == nil || c.log == nil {
		return
	}
	c.log.Debug().Msgf("[rcheevos] %s", C.GoString(message))
}

//export rcheevos_login_complete_bridge
func rcheevos_login_complete_bridge(result C.int, errorMessage *C.char, client *C.rc_client_t, userdata unsafe.Pointer) {
	h := cgo.Handle(uintptr(userdata))
	if h == 0 {
		return
	}
	c, ok := h.Value().(*Client)
	if !ok || c == nil {
		return
	}
	c.finishLogin(result, errorMessage)
}

//export rcheevos_load_game_complete_bridge
func rcheevos_load_game_complete_bridge(result C.int, errorMessage *C.char, client *C.rc_client_t, userdata unsafe.Pointer) {
	h := cgo.Handle(uintptr(userdata))
	if h == 0 {
		return
	}
	c, ok := h.Value().(*Client)
	if !ok || c == nil {
		return
	}
	c.finishLoadGame(result, errorMessage)
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// userAgent identifies the integration to retroachievements.org
// (Cloudflare in front of the RA API blocks the default Go UA).
var userAgent = "cloudplay/1.0 rcheevos/" + Version()

//export rcheevos_server_call_bridge
func rcheevos_server_call_bridge(request *C.rc_api_request_t, callback C.rc_client_server_callback_t, callbackData unsafe.Pointer, client *C.rc_client_t) {
	url := C.GoString(request.url)
	var body io.Reader
	method := http.MethodGet
	var contentType string
	if request.post_data != nil {
		postData := C.GoString(request.post_data)
		body = bytes.NewBufferString(postData)
		method = http.MethodPost
		if request.content_type != nil {
			contentType = C.GoString(request.content_type)
		} else {
			contentType = "application/x-www-form-urlencoded"
		}
	}

	// HTTP in a goroutine so the caller (rc_client) isn't blocked.
	// The server callback will fire back into rc_client when the
	// response lands; rc_client is expected to be re-entrant on its
	// own state here.
	go func() {
		status := 0
		var respBody []byte
		defer func() {
			invokeServerCallback(callback, callbackData, respBody, status)
		}()

		req, err := http.NewRequest(method, url, body)
		if err != nil {
			return
		}
		req.Header.Set("User-Agent", userAgent)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		status = resp.StatusCode
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return
		}
		respBody = b
	}()
}

// invokeServerCallback hands the HTTP response back to rc_client.
func invokeServerCallback(callback C.rc_client_server_callback_t, callbackData unsafe.Pointer, body []byte, status int) {
	var cbody *C.char
	var blen C.size_t
	if len(body) > 0 {
		cbody = (*C.char)(unsafe.Pointer(&body[0]))
		blen = C.size_t(len(body))
	}
	C.rcheevos_invoke_server_callback(callback, callbackData, cbody, blen, C.int(status))
}

// Helpers called from Go (bridge.go + rcheevos.go) that either (a)
// take addresses of Go-exported functions and pass them to rc_client
// as C function pointers, or (b) fabricate a rc_api_server_response_t
// on the stack so Go doesn't have to reach into cgo struct internals.

#include <stdlib.h>
#include <string.h>
#include <rc_client.h>

// Go-exported bridges (implemented in bridge.go via //export).
extern uint32_t rcheevos_read_memory_bridge(uint32_t address, uint8_t* buffer, uint32_t num_bytes, rc_client_t* client);
extern void     rcheevos_server_call_bridge(const rc_api_request_t* request, rc_client_server_callback_t callback, void* callback_data, rc_client_t* client);
extern void     rcheevos_log_bridge(const char* message, const rc_client_t* client);
extern void     rcheevos_login_complete_bridge(int result, const char* error_message, rc_client_t* client, void* userdata);

// rcheevos_create creates an rc_client with our Go-side callbacks wired.
rc_client_t* rcheevos_create(void) {
    rc_client_t* c = rc_client_create(rcheevos_read_memory_bridge, rcheevos_server_call_bridge);
    if (c != NULL) {
        rc_client_enable_logging(c, RC_CLIENT_LOG_LEVEL_INFO, rcheevos_log_bridge);
    }
    return c;
}

// rcheevos_begin_login kicks off rc_client login with a cached
// session token. userdata is a cgo.Handle for completion routing.
void rcheevos_begin_login(rc_client_t* client, const char* username, const char* token, uintptr_t userdata) {
    rc_client_begin_login_with_token(client, username, token,
        rcheevos_login_complete_bridge, (void*)userdata);
}

// rcheevos_begin_login_password exchanges a username+password for a
// session token. rc_client caches the token internally so subsequent
// calls (achievement loads, unlock posts) don't need the password.
void rcheevos_begin_login_password(rc_client_t* client, const char* username, const char* password, uintptr_t userdata) {
    rc_client_begin_login_with_password(client, username, password,
        rcheevos_login_complete_bridge, (void*)userdata);
}

// rcheevos_invoke_server_callback is called from Go after HTTP
// completes. It fabricates a stack-allocated response and feeds it to
// the rc_client callback that was handed to us in the original
// server_call bridge.
void rcheevos_invoke_server_callback(rc_client_server_callback_t cb, void* callback_data,
                                     const char* body, size_t body_length, int http_status_code) {
    rc_api_server_response_t resp = {
        .body = body,
        .body_length = body_length,
        .http_status_code = http_status_code,
    };
    cb(&resp, callback_data);
}

// Helpers called from Go (bridge.go + rcheevos.go) that either (a)
// take addresses of Go-exported functions and pass them to rc_client
// as C function pointers, or (b) fabricate a rc_api_server_response_t
// on the stack so Go doesn't have to reach into cgo struct internals.

#include <stdlib.h>
#include <string.h>
#include <rc_client.h>
#include <rc_hash.h>

// Go-exported bridges (implemented in bridge.go via //export).
extern uint32_t rcheevos_read_memory_bridge(uint32_t address, uint8_t* buffer, uint32_t num_bytes, rc_client_t* client);
extern void     rcheevos_server_call_bridge(const rc_api_request_t* request, rc_client_server_callback_t callback, void* callback_data, rc_client_t* client);
extern void     rcheevos_log_bridge(const char* message, const rc_client_t* client);
extern void     rcheevos_login_complete_bridge(int result, const char* error_message, rc_client_t* client, void* userdata);
extern void     rcheevos_load_game_complete_bridge(int result, const char* error_message, rc_client_t* client, void* userdata);
extern void     rcheevos_on_achievement_triggered(uint32_t id, const char* title, const char* description, uint32_t points, const char* badge_url, rc_client_t* client);

// rcheevos_event_handler routes rc_client events to Go. We only
// forward ACHIEVEMENT_TRIGGERED for now; leaderboards / challenge
// indicators / game-completed can light up as UI surfaces for them
// land. Called on the emulator thread during rc_client_do_frame.
static void rcheevos_event_handler(const rc_client_event_t* event, rc_client_t* client) {
    if (event->type == RC_CLIENT_EVENT_ACHIEVEMENT_TRIGGERED && event->achievement) {
        rcheevos_on_achievement_triggered(
            event->achievement->id,
            event->achievement->title,
            event->achievement->description,
            event->achievement->points,
            event->achievement->badge_url,
            client);
    }
}

// rcheevos_create creates an rc_client with our Go-side callbacks wired.
rc_client_t* rcheevos_create(void) {
    rc_client_t* c = rc_client_create(rcheevos_read_memory_bridge, rcheevos_server_call_bridge);
    if (c != NULL) {
        rc_client_enable_logging(c, RC_CLIENT_LOG_LEVEL_INFO, rcheevos_log_bridge);
        rc_client_set_event_handler(c, rcheevos_event_handler);
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

// rcheevos_hash_file computes the RA hash for a ROM at path, given a
// console id. Writes a 32-char hex string + NUL to out_hash (33 bytes).
// Returns RC_OK on success.
int rcheevos_hash_file(uint32_t console_id, const char* path, char* out_hash) {
    return rc_hash_generate_from_file(out_hash, console_id, path);
}

// rcheevos_begin_load_game kicks off load-game for the current hash.
// rc_client fetches the achievement set + unlock state asynchronously;
// completion routes to rcheevos_load_game_complete_bridge via userdata.
void rcheevos_begin_load_game(rc_client_t* client, const char* hash, uintptr_t userdata) {
    rc_client_begin_load_game(client, hash, rcheevos_load_game_complete_bridge, (void*)userdata);
}

#define _GNU_SOURCE
#include "libretro.h"
#include <pthread.h>
#include <signal.h>
#include <stdbool.h>
#include <stdarg.h>
#include <stdio.h>
#include <string.h>
#include <dlfcn.h>
#include <ucontext.h>
#include <stdint.h>
#include <stdlib.h>

// ── SIGSEGV crash handler with dladdr backtrace ──────────────────────────
// Installed before context_reset to capture the exact crash location when
// Dolphin's Vulkan init dereferences a bad pointer (addr=0x2).
// Prints: faulting address, crashing PC + library/function, and a backtrace.
// Then re-raises SIGSEGV so the process terminates normally.

static struct sigaction g_old_sigsegv_action;
static volatile int g_crash_handler_installed = 0;

static void cloudplay_sigsegv_handler(int sig, siginfo_t *si, void *ucontext_raw) {
    ucontext_t *uc = (ucontext_t *)ucontext_raw;

    fprintf(stderr, "\n╔══════════════════════════════════════════════════════════════╗\n");
    fprintf(stderr, "║  CLOUDPLAY SIGSEGV CRASH HANDLER                           ║\n");
    fprintf(stderr, "╚══════════════════════════════════════════════════════════════╝\n");
    fprintf(stderr, "Signal: %d (SIGSEGV)\n", sig);
    fprintf(stderr, "Faulting address (si_addr): %p\n", si->si_addr);
    fprintf(stderr, "Signal code (si_code): %d", si->si_code);
    switch (si->si_code) {
        case SEGV_MAPERR: fprintf(stderr, " (SEGV_MAPERR - address not mapped)\n"); break;
        case SEGV_ACCERR: fprintf(stderr, " (SEGV_ACCERR - invalid permissions)\n"); break;
        default: fprintf(stderr, " (unknown)\n"); break;
    }

    // Extract the program counter (instruction that faulted)
    void *pc = NULL;
#if defined(__x86_64__)
    if (uc) pc = (void *)uc->uc_mcontext.gregs[REG_RIP];
#elif defined(__aarch64__)
    if (uc) pc = (void *)uc->uc_mcontext.pc;
#endif
    fprintf(stderr, "Crashing PC: %p\n", pc);

    // dladdr on the faulting address
    Dl_info info_addr;
    if (dladdr(si->si_addr, &info_addr)) {
        fprintf(stderr, "Faulting addr resolves to: %s (%s+%p)\n",
            info_addr.dli_sname ? info_addr.dli_sname : "(unknown)",
            info_addr.dli_fname ? info_addr.dli_fname : "(unknown)",
            (void *)((char *)si->si_addr - (char *)info_addr.dli_saddr));
    } else {
        fprintf(stderr, "Faulting addr: dladdr failed (unmapped)\n");
    }

    // dladdr on the crashing instruction
    if (pc) {
        Dl_info info_pc;
        if (dladdr(pc, &info_pc)) {
            fprintf(stderr, "Crashing PC resolves to: %s in %s (base=%p, sym=%p, offset=+0x%lx)\n",
                info_pc.dli_sname ? info_pc.dli_sname : "(unknown)",
                info_pc.dli_fname ? info_pc.dli_fname : "(unknown)",
                info_pc.dli_fbase,
                info_pc.dli_saddr,
                info_pc.dli_saddr ? (unsigned long)((char *)pc - (char *)info_pc.dli_saddr) : 0);
        } else {
            fprintf(stderr, "Crashing PC: dladdr failed\n");
        }
    }

    // Manual frame-pointer backtrace (works without glibc backtrace())
    fprintf(stderr, "\n--- Backtrace (frame-pointer walk, up to 64 frames) ---\n");
#if defined(__x86_64__)
    {
        void **fp = NULL;
        if (uc) fp = (void **)uc->uc_mcontext.gregs[REG_RBP];
        // First frame is the crashing PC itself
        if (pc) {
            Dl_info fi;
            if (dladdr(pc, &fi)) {
                fprintf(stderr, "  #00 %p %s+0x%lx [%s]\n", pc,
                    fi.dli_sname ? fi.dli_sname : "??",
                    fi.dli_saddr ? (unsigned long)((char*)pc - (char*)fi.dli_saddr) : 0,
                    fi.dli_fname ? fi.dli_fname : "??");
            } else {
                fprintf(stderr, "  #00 %p (dladdr failed)\n", pc);
            }
        }
        for (int i = 1; i < 64 && fp != NULL; i++) {
            void *ret_addr = fp[1];
            if (ret_addr == NULL || (unsigned long)ret_addr < 0x1000) break;
            Dl_info fi;
            if (dladdr(ret_addr, &fi)) {
                fprintf(stderr, "  #%02d %p %s+0x%lx [%s]\n", i, ret_addr,
                    fi.dli_sname ? fi.dli_sname : "??",
                    fi.dli_saddr ? (unsigned long)((char*)ret_addr - (char*)fi.dli_saddr) : 0,
                    fi.dli_fname ? fi.dli_fname : "??");
            } else {
                fprintf(stderr, "  #%02d %p (dladdr failed)\n", i, ret_addr);
            }
            void **next_fp = (void **)fp[0];
            if (next_fp <= fp) break; // stack must grow upward in frames
            fp = next_fp;
        }
    }
#else
    fprintf(stderr, "  (frame-pointer walk not implemented for this arch)\n");
#endif

    // Print register context on x86_64 for detailed debugging
#if defined(__x86_64__)
    if (uc) {
        fprintf(stderr, "\n--- Registers ---\n");
        fprintf(stderr, "RAX=%016llx RBX=%016llx RCX=%016llx RDX=%016llx\n",
            (unsigned long long)uc->uc_mcontext.gregs[REG_RAX],
            (unsigned long long)uc->uc_mcontext.gregs[REG_RBX],
            (unsigned long long)uc->uc_mcontext.gregs[REG_RCX],
            (unsigned long long)uc->uc_mcontext.gregs[REG_RDX]);
        fprintf(stderr, "RSI=%016llx RDI=%016llx RBP=%016llx RSP=%016llx\n",
            (unsigned long long)uc->uc_mcontext.gregs[REG_RSI],
            (unsigned long long)uc->uc_mcontext.gregs[REG_RDI],
            (unsigned long long)uc->uc_mcontext.gregs[REG_RBP],
            (unsigned long long)uc->uc_mcontext.gregs[REG_RSP]);
        fprintf(stderr, "R8 =%016llx R9 =%016llx R10=%016llx R11=%016llx\n",
            (unsigned long long)uc->uc_mcontext.gregs[REG_R8],
            (unsigned long long)uc->uc_mcontext.gregs[REG_R9],
            (unsigned long long)uc->uc_mcontext.gregs[REG_R10],
            (unsigned long long)uc->uc_mcontext.gregs[REG_R11]);
        fprintf(stderr, "R12=%016llx R13=%016llx R14=%016llx R15=%016llx\n",
            (unsigned long long)uc->uc_mcontext.gregs[REG_R12],
            (unsigned long long)uc->uc_mcontext.gregs[REG_R13],
            (unsigned long long)uc->uc_mcontext.gregs[REG_R14],
            (unsigned long long)uc->uc_mcontext.gregs[REG_R15]);
        fprintf(stderr, "RIP=%016llx\n",
            (unsigned long long)uc->uc_mcontext.gregs[REG_RIP]);
    }
#endif

    fprintf(stderr, "\n╔══════════════════════════════════════════════════════════════╗\n");
    fprintf(stderr, "║  END CRASH HANDLER — re-raising SIGSEGV                    ║\n");
    fprintf(stderr, "╚══════════════════════════════════════════════════════════════╝\n");
    fflush(stderr);

    // Restore old handler and re-raise
    sigaction(SIGSEGV, &g_old_sigsegv_action, NULL);
    raise(SIGSEGV);
}

void cloudplay_install_crash_handler(void) {
    if (g_crash_handler_installed) return;
    struct sigaction sa;
    memset(&sa, 0, sizeof(sa));
    sa.sa_sigaction = cloudplay_sigsegv_handler;
    sa.sa_flags = SA_SIGINFO | SA_ONSTACK;
    sigemptyset(&sa.sa_mask);
    sigaction(SIGSEGV, &sa, &g_old_sigsegv_action);
    g_crash_handler_installed = 1;
    fprintf(stderr, "[cloudplay] SIGSEGV crash handler installed\n");
}

void cloudplay_remove_crash_handler(void) {
    if (!g_crash_handler_installed) return;
    sigaction(SIGSEGV, &g_old_sigsegv_action, NULL);
    g_crash_handler_installed = 0;
    fprintf(stderr, "[cloudplay] SIGSEGV crash handler removed\n");
}

#define RETRO_ENVIRONMENT_GET_CLEAR_ALL_THREAD_WAITS_CB (3 | 0x800000)

int initialized = 0;

typedef struct {
    int   type;
    void* fn;
    void* arg1;
    void* arg2;
    void* result;
} call_def_t;

call_def_t call;

enum call_type {
    CALL_VOID = -1,
    CALL_SERIALIZE = 1,
    CALL_UNSERIALIZE = 2,
};

void *same_thread_with_args(void *f, int type, ...);

// Input State Cache

#define INPUT_MAX_PORTS 8
#define INPUT_MAX_KEYS 512

typedef struct {
    uint32_t buttons[INPUT_MAX_PORTS];
    int16_t analog[INPUT_MAX_PORTS][4];     // LX, LY, RX, RY
    int16_t triggers[INPUT_MAX_PORTS][2];   // L2, R2

    uint8_t keyboard[INPUT_MAX_KEYS];
    int16_t mouse_x;
    int16_t mouse_y;
    uint8_t mouse_buttons;
} input_cache_t;

static input_cache_t input_cache = {0};

// Update entire port state at once
static unsigned g_set_port_diag = 0;

void input_cache_set_port(unsigned port, uint32_t buttons,
                          int16_t lx, int16_t ly, int16_t rx, int16_t ry,
                          int16_t l2, int16_t r2) {
    if (port < INPUT_MAX_PORTS) {
        if (port == 0 && (++g_set_port_diag <= 10 || (buttons != 0 && g_set_port_diag % 60 == 0))) {
            fprintf(stderr, "[DIAG C set_port] port=%u buttons=0x%08x lx=%d ly=%d rx=%d ry=%d l2=%d r2=%d\n",
                    port, buttons, lx, ly, rx, ry, l2, r2);
        }
        input_cache.buttons[port] = buttons;
        input_cache.analog[port][0] = lx;
        input_cache.analog[port][1] = ly;
        input_cache.analog[port][2] = rx;
        input_cache.analog[port][3] = ry;
        input_cache.triggers[port][0] = l2;
        input_cache.triggers[port][1] = r2;
    }
}

// Keyboard update
void input_cache_set_keyboard_key(unsigned id, uint8_t pressed) {
    if (id < INPUT_MAX_KEYS) {
        input_cache.keyboard[id] = pressed;
    }
}

// Mouse update
void input_cache_set_mouse(int16_t dx, int16_t dy, uint8_t buttons) {
    input_cache.mouse_x = dx;
    input_cache.mouse_y = dy;
    input_cache.mouse_buttons = buttons;
}

void input_cache_clear(void) {
    memset(&input_cache, 0, sizeof(input_cache));
}

void core_log_cgo(enum retro_log_level level, const char *fmt, ...) {
    char msg[2048] = {0};
    va_list va;
    va_start(va, fmt);
    vsnprintf(msg, sizeof(msg), fmt, va);
    va_end(va);
    void coreLog(enum retro_log_level level, const char *msg);
    coreLog(level, msg);
}

void bridge_call(void *f) {
    ((void (*)(void)) f)();
}

void bridge_set_callback(void *f, void *callback) {
    ((void (*)(void *))f)(callback);
}

unsigned bridge_retro_api_version(void *f) {
    return ((unsigned (*)(void)) f)();
}

void bridge_retro_get_system_info(void *f, struct retro_system_info *si) {
    ((void (*)(struct retro_system_info *)) f)(si);
}

void bridge_retro_get_system_av_info(void *f, struct retro_system_av_info *si) {
    ((void (*)(struct retro_system_av_info *)) f)(si);
}

bool bridge_retro_set_environment(void *f, void *callback) {
    return ((bool (*)(retro_environment_t)) f)((retro_environment_t) callback);
}

void bridge_retro_set_input_state(void *f, void *callback) {
    ((int16_t (*)(retro_input_state_t)) f)((retro_input_state_t) callback);
}

bool bridge_retro_load_game(void *f, struct retro_game_info *gi) {
    return ((bool (*)(struct retro_game_info *)) f)(gi);
}

size_t bridge_retro_get_memory_size(void *f, unsigned id) {
    return ((size_t (*)(unsigned)) f)(id);
}

void *bridge_retro_get_memory_data(void *f, unsigned id) {
    return ((void *(*)(unsigned)) f)(id);
}

size_t bridge_retro_serialize_size(void *f) {
    return ((size_t (*)(void)) f)();
}

bool bridge_retro_serialize(void *f, void *data, size_t size) {
    return ((bool (*)(void *, size_t)) f)(data, size);
}

bool bridge_retro_unserialize(void *f, void *data, size_t size) {
    return ((bool (*)(void *, size_t)) f)(data, size);
}

void bridge_retro_set_controller_port_device(void *f, unsigned port, unsigned device) {
    ((void (*)(unsigned, unsigned)) f)(port, device);
}

static bool clear_all_thread_waits_cb(unsigned v, void *data) {
    core_log_cgo(RETRO_LOG_DEBUG, "CLEAR_ALL_THREAD_WAITS_CB (%d)\n", v);
    return true;
}

void bridge_retro_keyboard_callback(void *cb, bool down, unsigned keycode, uint32_t character, uint16_t keyModifiers) {
    (*(retro_keyboard_event_t *) cb)(down, keycode, character, keyModifiers);
}

bool core_environment_cgo(unsigned cmd, void *data) {
    bool coreEnvironment(unsigned, void *);

    switch (cmd)
    {
        case RETRO_ENVIRONMENT_GET_VARIABLE_UPDATE:
          return false;
          break;
        case RETRO_ENVIRONMENT_GET_AUDIO_VIDEO_ENABLE:
          return false;
          break;
        case RETRO_ENVIRONMENT_GET_CLEAR_ALL_THREAD_WAITS_CB:
          *(retro_environment_t *)data = clear_all_thread_waits_cb;
          return true;
          break;
        case RETRO_ENVIRONMENT_GET_INPUT_MAX_USERS:
          *(unsigned *)data = INPUT_MAX_PORTS;
          core_log_cgo(RETRO_LOG_DEBUG, "Set max users: %d\n", INPUT_MAX_PORTS);
          return true;
          break;
        case RETRO_ENVIRONMENT_GET_INPUT_BITMASKS:
          return true;
        case RETRO_ENVIRONMENT_SHUTDOWN:
          return false;
          break;
        case RETRO_ENVIRONMENT_GET_SAVESTATE_CONTEXT:
          if (data != NULL) *(int *)data = RETRO_SAVESTATE_CONTEXT_NORMAL;
          return true;
          break;
    }

    return coreEnvironment(cmd, data);
}

static int g_video_refresh_count = 0;

void core_video_refresh_cgo(void *data, unsigned width, unsigned height, size_t pitch) {
    g_video_refresh_count++;

    // DIAG: inspect data in C before Go ever touches it.
    // This isolates whether the emulator itself writes zeros, or if it's a
    // CGo / Go memory visibility issue.
    // Dense sampling: first 10 frames, then every 60th.
    if (data != NULL && data != (void*)(uintptr_t)-1 /* RETRO_HW_FRAME_BUFFER_VALID */
        && (g_video_refresh_count <= 10 || g_video_refresh_count % 60 == 0)) {
        size_t bytes = pitch * height;
        unsigned char *p = (unsigned char *)data;
        // Count non-zero bytes in entire frame (up to 100 to avoid perf hit)
        int nonzero_total = 0;
        for (size_t i = 0; i < bytes && nonzero_total < 100; i++) {
            if (p[i] != 0) nonzero_total++;
        }
        // Sample pixel data AND padding for each row to determine where
        // the non-zero bytes actually are.
        // For width=256, bpp=2: pixel data = bytes 0-511, padding = bytes 512-1023
        size_t pixel_bytes = width * 2; // assume bpp=2 for now
        size_t mid_row = (height / 2) * pitch;

        // Count non-zero in pixel area vs padding area for mid row
        int pix_nz = 0, pad_nz = 0;
        if (mid_row + pitch <= bytes) {
            for (size_t i = mid_row; i < mid_row + pixel_bytes && i < mid_row + pitch; i++) {
                if (p[i] != 0) pix_nz++;
            }
            for (size_t i = mid_row + pixel_bytes; i < mid_row + pitch; i++) {
                if (p[i] != 0) pad_nz++;
            }
        }

        fprintf(stderr, "[DIAG C core_video_refresh] frame=%d ptr=%p w=%u h=%u pitch=%zu bytes=%zu nz_total=%d "
            "mid_pix_nz=%d mid_pad_nz=%d "
            "row0_pix=[%02x%02x %02x%02x] "
            "row0_pad=[%02x%02x %02x%02x] "
            "mid_pix=[%02x%02x %02x%02x] "
            "mid_pad=[%02x%02x %02x%02x]\n",
            g_video_refresh_count, data, width, height, pitch, bytes, nonzero_total,
            pix_nz, pad_nz,
            p[0], p[1], p[2], p[3],
            pixel_bytes < pitch ? p[pixel_bytes] : 0, pixel_bytes+1 < pitch ? p[pixel_bytes+1] : 0,
            pixel_bytes+2 < pitch ? p[pixel_bytes+2] : 0, pixel_bytes+3 < pitch ? p[pixel_bytes+3] : 0,
            mid_row < bytes ? p[mid_row] : 0, mid_row+1 < bytes ? p[mid_row+1] : 0,
            mid_row+2 < bytes ? p[mid_row+2] : 0, mid_row+3 < bytes ? p[mid_row+3] : 0,
            mid_row+pixel_bytes < bytes ? p[mid_row+pixel_bytes] : 0,
            mid_row+pixel_bytes+1 < bytes ? p[mid_row+pixel_bytes+1] : 0,
            mid_row+pixel_bytes+2 < bytes ? p[mid_row+pixel_bytes+2] : 0,
            mid_row+pixel_bytes+3 < bytes ? p[mid_row+pixel_bytes+3] : 0);
    }

    void coreVideoRefresh(void *, unsigned, unsigned, size_t);
    coreVideoRefresh(data, width, height, pitch);
}

void core_input_poll_cgo() {
}

static unsigned g_input_diag_count = 0;

int16_t core_input_state_cgo(unsigned port, unsigned device, unsigned index, unsigned id) {
    if (port >= INPUT_MAX_PORTS) {
        return 0;
    }

    switch (device) {
        case RETRO_DEVICE_JOYPAD:
            if (id == RETRO_DEVICE_ID_JOYPAD_MASK) {
                int16_t result = (int16_t)(input_cache.buttons[port] & 0xFFFF);
                if (port == 0 && (++g_input_diag_count <= 30 || (result != 0 && g_input_diag_count % 60 == 0))) {
                    fprintf(stderr, "[DIAG C input_state] port=%u JOYPAD_MASK buttons_raw=0x%08x result=0x%04x\n",
                            port, input_cache.buttons[port], (unsigned)(result & 0xFFFF));
                }
                return result;
            }
            return (int16_t)((input_cache.buttons[port] >> id) & 1);

        case RETRO_DEVICE_ANALOG:
            switch (index) {
                case RETRO_DEVICE_INDEX_ANALOG_LEFT:
                    // id: RETRO_DEVICE_ID_ANALOG_X=0, RETRO_DEVICE_ID_ANALOG_Y=1
                    if (id <= RETRO_DEVICE_ID_ANALOG_Y) {
                        return input_cache.analog[port][id];
                    }
                    break;
                case RETRO_DEVICE_INDEX_ANALOG_RIGHT:
                    // id: RETRO_DEVICE_ID_ANALOG_X=0, RETRO_DEVICE_ID_ANALOG_Y=1
                    if (id <= RETRO_DEVICE_ID_ANALOG_Y) {
                        return input_cache.analog[port][2 + id];
                    }
                    break;
                case RETRO_DEVICE_INDEX_ANALOG_BUTTON:
                    // Any button can be queried as analog
                    // id = RETRO_DEVICE_ID_JOYPAD_* (0-15)
                    // For now, only L2/R2 have analog values
                    switch (id) {
                        case RETRO_DEVICE_ID_JOYPAD_L2:
                            return input_cache.triggers[port][0];
                        case RETRO_DEVICE_ID_JOYPAD_R2:
                            return input_cache.triggers[port][1];
                        default:
                            // Other buttons: return digital as 0 or 0x7fff
                            return ((input_cache.buttons[port] >> id) & 1) ? 0x7FFF : 0;
                    }
                    break;
            }
            break;

        case RETRO_DEVICE_KEYBOARD:
            if (id < INPUT_MAX_KEYS) {
                return input_cache.keyboard[id] ? 1 : 0;
            }
            break;

        case RETRO_DEVICE_MOUSE:
            switch (id) {
                case RETRO_DEVICE_ID_MOUSE_X: {
                    int16_t x = input_cache.mouse_x;
                    input_cache.mouse_x = 0;
                    return x;
                }
                case RETRO_DEVICE_ID_MOUSE_Y: {
                    int16_t y = input_cache.mouse_y;
                    input_cache.mouse_y = 0;
                    return y;
                }
                case RETRO_DEVICE_ID_MOUSE_LEFT:
                    return (input_cache.mouse_buttons & 0x01) ? 1 : 0;
                case RETRO_DEVICE_ID_MOUSE_RIGHT:
                    return (input_cache.mouse_buttons & 0x02) ? 1 : 0;
                case RETRO_DEVICE_ID_MOUSE_MIDDLE:
                    return (input_cache.mouse_buttons & 0x04) ? 1 : 0;
            }
            break;
    }

    return 0;
}

size_t core_audio_sample_batch_cgo(const int16_t *data, size_t frames) {
    size_t coreAudioSampleBatch(const int16_t *, size_t);
    return coreAudioSampleBatch(data, frames);
}

void core_audio_sample_cgo(int16_t left, int16_t right) {
    int16_t frame[2] = { left, right };
    core_audio_sample_batch_cgo(frame, 1);
}

uintptr_t core_get_current_framebuffer_cgo() {
    uintptr_t coreGetCurrentFramebuffer();
    return coreGetCurrentFramebuffer();
}

retro_proc_address_t core_get_proc_address_cgo(const char *sym) {
    retro_proc_address_t coreGetProcAddress(const char *sym);
    return coreGetProcAddress(sym);
}

void bridge_context_reset(retro_hw_context_reset_t f) {
    f();
}

void init_video_cgo() {
    void initVideo();
    initVideo();
}

void deinit_video_cgo() {
    void deinitVideo();
    deinitVideo();
}

typedef struct {
   pthread_mutex_t m;
   pthread_cond_t cond;
} mutex_t;

void mutex_init(mutex_t *m) {
    pthread_mutex_init(&m->m, NULL);
    pthread_cond_init(&m->cond, NULL);
}

void mutex_destroy(mutex_t *m) {
    pthread_mutex_trylock(&m->m);
    pthread_mutex_unlock(&m->m);
    pthread_mutex_destroy(&m->m);
    pthread_cond_signal(&m->cond);
    pthread_cond_destroy(&m->cond);
}

void mutex_lock(mutex_t *m)   { pthread_mutex_lock(&m->m); }
void mutex_wait(mutex_t *m)   { pthread_cond_wait(&m->cond, &m->m); }
void mutex_unlock(mutex_t *m) { pthread_mutex_unlock(&m->m); }
void mutex_signal(mutex_t *m) { pthread_cond_signal(&m->cond); }

static pthread_t thread;
mutex_t run_mutex, done_mutex;

void *run_loop(void *unused) {
    // Unblock SIGBUS and SIGSEGV so that Dolphin's JIT fastmem signal
    // handlers can handle memory-mapped access faults in GameCube emulation.
    // Without this, Go's runtime (which manages signal dispositions for all
    // threads in the process) may intercept these signals and panic instead
    // of forwarding them to Dolphin's registered sigaction handler.
    {
        sigset_t unblock;
        sigemptyset(&unblock);
        sigaddset(&unblock, SIGBUS);
        sigaddset(&unblock, SIGSEGV);
        pthread_sigmask(SIG_UNBLOCK, &unblock, NULL);
    }
    core_log_cgo(RETRO_LOG_DEBUG, "UnLibCo run loop start\n");
    mutex_lock(&done_mutex);
    mutex_lock(&run_mutex);
    mutex_signal(&done_mutex);
    mutex_unlock(&done_mutex);
    while (initialized) {
        mutex_wait(&run_mutex);
        switch (call.type) {
            case CALL_SERIALIZE:
            case CALL_UNSERIALIZE:
              *(bool*)call.result = ((bool (*)(void*, size_t))call.fn)(call.arg1, *(size_t*)call.arg2);
              break;
            default:
                ((void (*)(void)) call.fn)();
        }
        mutex_lock(&done_mutex);
        mutex_signal(&done_mutex);
        mutex_unlock(&done_mutex);
    }
    mutex_destroy(&run_mutex);
    mutex_destroy(&done_mutex);
    pthread_detach(thread);
    core_log_cgo(RETRO_LOG_DEBUG, "UnLibCo run loop stop\n");
    pthread_exit(NULL);
}

void same_thread_stop() {
    initialized = 0;
}

void *same_thread_with_args(void *f, int type, ...) {
    if (!initialized) {
        initialized = 1;
        mutex_init(&run_mutex);
        mutex_init(&done_mutex);
        mutex_lock(&done_mutex);
        pthread_create(&thread, NULL, run_loop, NULL);
        mutex_wait(&done_mutex);
        mutex_unlock(&done_mutex);
    }
    mutex_lock(&run_mutex);
    mutex_lock(&done_mutex);

    call.type = type;
    call.fn = f;

    if (type != CALL_VOID) {
        va_list args;
        va_start(args, type);
        switch (type) {
            case CALL_SERIALIZE:
            case CALL_UNSERIALIZE:
                call.arg1 = va_arg(args, void*);
                size_t size;
                size = va_arg(args, size_t);
                call.arg2 = &size;
                bool result;
                call.result = &result;
              break;
        }
        va_end(args);
    }
    mutex_signal(&run_mutex);
    mutex_unlock(&run_mutex);
    mutex_wait(&done_mutex);
    mutex_unlock(&done_mutex);
    return call.result;
}

void *same_thread_with_args2(void *f, int type, void *arg1, void *arg2) {
    return same_thread_with_args(f, type, arg1, arg2);
}

void same_thread(void *f) {
    same_thread_with_args(f, CALL_VOID);
}

bool core_rumble_cgo(unsigned port, enum retro_rumble_effect effect, uint16_t strength) {
    bool coreRumble(unsigned, unsigned, uint16_t);
    static int rumble_diag_count = 0;
    if (++rumble_diag_count <= 20 || (strength > 0 && rumble_diag_count % 60 == 0)) {
        fprintf(stderr, "[DIAG core_rumble_cgo] port=%u effect=%u strength=%u call=%d\n", port, (unsigned)effect, strength, rumble_diag_count);
    }
    return coreRumble(port, (unsigned)effect, strength);
}

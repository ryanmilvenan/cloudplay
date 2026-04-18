// videocap_preload.c — LD_PRELOAD shim that captures GL frames out of xemu
// (or any GLX-using process) and ships them over a Unix socket to the Go
// receiver in videocap.go.
//
// Design:
//   * Intercept glXSwapBuffers. Before forwarding to the real implementation,
//     read the backbuffer via glReadPixels (RGBA / UNSIGNED_BYTE) and write
//     a framed message to the socket.
//   * Socket path comes from the env var CLOUDPLAY_VIDEOCAP_SOCKET.
//     If unset or the connection fails, the shim is a no-op wrapper — xemu
//     keeps running, we just don't capture frames.
//   * One connection per process lifetime. Reconnects are a future problem.
//
// Message format (little-endian, matches runtime.GOARCH amd64 native):
//
//   magic   uint32    0x56524d46  ('FMRV' read as LE)
//   width   uint32
//   height  uint32
//   stride  uint32    width*4 (RGBA, no padding)
//   format  uint32    0 = RGBA8
//   seq     uint32    monotonic frame number within this process
//   length  uint32    width*height*4 (bytes of payload to follow)
//   payload <length> bytes of RGBA pixels, bottom-up from glReadPixels
//
// Build: gcc -shared -fPIC -O2 videocap_preload.c -ldl -lGL -lpthread
//            -o videocap_preload.so
// Load:  LD_PRELOAD=/path/videocap_preload.so
//        CLOUDPLAY_VIDEOCAP_SOCKET=/tmp/xemu-cap.sock xemu

#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

// Opaque types — we never dereference these, just forward them.
typedef unsigned long XID;
typedef XID GLXDrawable;
struct _XDisplay;
typedef struct _XDisplay Display;

// GL types we do touch.
typedef unsigned int GLenum;
typedef int          GLint;
typedef int          GLsizei;
typedef void         GLvoid;

#define GL_RGBA           0x1908
#define GL_UNSIGNED_BYTE  0x1401
#define GL_VIEWPORT       0x0BA2
#define GL_BACK           0x0405

typedef void          (*glXSwapBuffers_t)(Display *dpy, GLXDrawable drw);
typedef unsigned int  (*eglSwapBuffers_t)(void *dpy, void *surface);
typedef int           (*SDL_GL_SwapWindow_t)(void *window);
typedef void          (*glReadBuffer_t)(GLenum mode);
typedef void          (*glReadPixels_t)(GLint x, GLint y, GLsizei w, GLsizei h,
                                        GLenum fmt, GLenum type, GLvoid *pixels);
typedef void          (*glGetIntegerv_t)(GLenum pname, GLint *params);
typedef void          (*glFinish_t)(void);

static glXSwapBuffers_t     real_glXSwapBuffers   = NULL;
static eglSwapBuffers_t     real_eglSwapBuffers   = NULL;
static SDL_GL_SwapWindow_t  real_SDL_GL_SwapWindow = NULL;
static glReadBuffer_t       sym_glReadBuffer      = NULL;
static glReadPixels_t       sym_glReadPixels      = NULL;
static glGetIntegerv_t      sym_glGetIntegerv     = NULL;
static glFinish_t           sym_glFinish          = NULL;

static int            g_sock         = -1;
static pthread_mutex_t g_sock_mu     = PTHREAD_MUTEX_INITIALIZER;
static uint32_t       g_seq          = 0;
static uint8_t *      g_buf          = NULL;
static size_t         g_buf_cap      = 0;
static int            g_debug        = 0;

#define VIDEOCAP_MAGIC  0x56524d46u  // 'FMRV' LE

static void dbg(const char *fmt, ...) {
    if (!g_debug) return;
    va_list ap;
    va_start(ap, fmt);
    fputs("[videocap_preload] ", stderr);
    vfprintf(stderr, fmt, ap);
    fputc('\n', stderr);
    va_end(ap);
}

// Resolve GL entry points. We use RTLD_NEXT for swap hooks (so our forwarder
// can find the "real" implementation below us in the load order) but
// RTLD_DEFAULT for read-side GL calls — xemu pulls them through libepoxy,
// which doesn't re-export glReadPixels/glGetIntegerv/etc as regular symbols,
// so RTLD_NEXT returns NULL. RTLD_DEFAULT searches the whole loaded-symbol
// table and finds libGL's versions (which libGL exports as weak/plain symbols).
//
// As a last-ditch fallback we dlopen libGL.so.1 directly with RTLD_NOW; on
// modern Ubuntu that's a simple redirect to libGLX_indirect or nvidia's GLX.
static void *g_libGL = NULL;

static void *try_sym(const char *name) {
    void *p = dlsym(RTLD_DEFAULT, name);
    if (p) return p;
    if (!g_libGL) {
        g_libGL = dlopen("libGL.so.1", RTLD_NOW | RTLD_NOLOAD);
        if (!g_libGL) g_libGL = dlopen("libGL.so.1", RTLD_NOW);
    }
    if (g_libGL) return dlsym(g_libGL, name);
    return NULL;
}

static void resolve_syms(void) {
    real_glXSwapBuffers    = (glXSwapBuffers_t)   dlsym(RTLD_NEXT, "glXSwapBuffers");
    real_eglSwapBuffers    = (eglSwapBuffers_t)   dlsym(RTLD_NEXT, "eglSwapBuffers");
    real_SDL_GL_SwapWindow = (SDL_GL_SwapWindow_t)dlsym(RTLD_NEXT, "SDL_GL_SwapWindow");
    sym_glReadBuffer       = (glReadBuffer_t)     try_sym("glReadBuffer");
    sym_glReadPixels       = (glReadPixels_t)     try_sym("glReadPixels");
    sym_glGetIntegerv      = (glGetIntegerv_t)    try_sym("glGetIntegerv");
    sym_glFinish           = (glFinish_t)         try_sym("glFinish");
    if (g_debug) {
        dbg("resolve: glXSwapBuffers=%p eglSwapBuffers=%p SDL_GL_SwapWindow=%p "
            "glReadPixels=%p glGetIntegerv=%p",
            (void*)real_glXSwapBuffers, (void*)real_eglSwapBuffers,
            (void*)real_SDL_GL_SwapWindow, (void*)sym_glReadPixels,
            (void*)sym_glGetIntegerv);
    }
}

static int connect_sock(const char *path) {
    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0) {
        dbg("socket: %s", strerror(errno));
        return -1;
    }
    // Set close-on-exec portably (SOCK_CLOEXEC is a Linux extension that
    // clang-on-mac lints against; fcntl works everywhere).
    int flags = fcntl(fd, F_GETFD);
    if (flags >= 0) (void)fcntl(fd, F_SETFD, flags | FD_CLOEXEC);
    struct sockaddr_un addr = {0};
    addr.sun_family = AF_UNIX;
    strncpy(addr.sun_path, path, sizeof(addr.sun_path) - 1);
    if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        dbg("connect %s: %s", path, strerror(errno));
        close(fd);
        return -1;
    }
    // Bump send buffer so a dropped frame here doesn't stall xemu.
    int sndbuf = 8 * 1024 * 1024;
    (void)setsockopt(fd, SOL_SOCKET, SO_SNDBUF, &sndbuf, sizeof(sndbuf));
    return fd;
}

__attribute__((constructor))
static void videocap_init(void) {
    if (getenv("CLOUDPLAY_VIDEOCAP_DEBUG")) g_debug = 1;
    resolve_syms();
    const char *path = getenv("CLOUDPLAY_VIDEOCAP_SOCKET");
    if (!path || !*path) {
        dbg("CLOUDPLAY_VIDEOCAP_SOCKET unset — pass-through mode");
        return;
    }
    g_sock = connect_sock(path);
    if (g_sock < 0) {
        dbg("capture disabled (socket connect failed)");
        return;
    }
    dbg("connected to %s (fd=%d)", path, g_sock);
}

// send_all loops until `len` bytes have been written or an unrecoverable
// error occurs. EAGAIN / EINTR are retried; anything else closes the socket
// and disables capture.
static int send_all(const uint8_t *buf, size_t len) {
    size_t off = 0;
    while (off < len) {
        ssize_t n = send(g_sock, buf + off, len - off, MSG_NOSIGNAL);
        if (n < 0) {
            if (errno == EINTR || errno == EAGAIN) continue;
            dbg("send: %s — closing socket", strerror(errno));
            close(g_sock);
            g_sock = -1;
            return -1;
        }
        off += (size_t)n;
    }
    return 0;
}

static uint64_t g_capture_skip_sock = 0;
static uint64_t g_capture_skip_syms = 0;
static uint64_t g_capture_skip_viewport = 0;
static uint64_t g_capture_ok = 0;

static void capture_frame(void) {
    if (g_sock < 0) { g_capture_skip_sock++; return; }
    if (!sym_glReadPixels || !sym_glGetIntegerv) { g_capture_skip_syms++; return; }

    GLint viewport[4] = {0, 0, 0, 0};
    sym_glGetIntegerv(GL_VIEWPORT, viewport);
    int x = viewport[0];
    int y = viewport[1];
    int w = viewport[2];
    int h = viewport[3];
    if (w <= 0 || h <= 0 || w > 4096 || h > 4096) {
        g_capture_skip_viewport++;
        if (g_debug && g_capture_skip_viewport <= 3) {
            dbg("skip viewport: %d %d %d %d", viewport[0], viewport[1], viewport[2], viewport[3]);
        }
        return;
    }

    size_t need = (size_t)w * (size_t)h * 4u;
    if (g_buf_cap < need) {
        uint8_t *n = realloc(g_buf, need);
        if (!n) return;
        g_buf = n;
        g_buf_cap = need;
    }

    if (sym_glReadBuffer) sym_glReadBuffer(GL_BACK);
    sym_glReadPixels(x, y, w, h, GL_RGBA, GL_UNSIGNED_BYTE, g_buf);

    uint32_t hdr[7];
    hdr[0] = VIDEOCAP_MAGIC;
    hdr[1] = (uint32_t)w;
    hdr[2] = (uint32_t)h;
    hdr[3] = (uint32_t)(w * 4);
    hdr[4] = 0; // RGBA8
    hdr[5] = g_seq++;
    hdr[6] = (uint32_t)need;

    pthread_mutex_lock(&g_sock_mu);
    if (g_sock >= 0) {
        if (send_all((const uint8_t *)hdr, sizeof(hdr)) == 0) {
            if (send_all(g_buf, need) == 0) {
                g_capture_ok++;
                if (g_debug && (g_capture_ok == 1 || g_capture_ok % 60 == 0)) {
                    dbg("captured %llu frames (%dx%d)",
                        (unsigned long long)g_capture_ok, w, h);
                }
            }
        }
    }
    pthread_mutex_unlock(&g_sock_mu);
}

__attribute__((destructor))
static void videocap_teardown(void) {
    dbg("teardown: sock_skip=%llu syms_skip=%llu viewport_skip=%llu ok=%llu",
        (unsigned long long)g_capture_skip_sock,
        (unsigned long long)g_capture_skip_syms,
        (unsigned long long)g_capture_skip_viewport,
        (unsigned long long)g_capture_ok);
    if (g_sock >= 0) close(g_sock);
}

static int g_hook_logged = 0;
static void log_first_hook(const char *which) {
    if (!g_debug || g_hook_logged) return;
    g_hook_logged = 1;
    dbg("first capture via %s", which);
}

// Our hooked glXSwapBuffers — legacy X11 GLX path.
void glXSwapBuffers(Display *dpy, GLXDrawable drw) {
    log_first_hook("glXSwapBuffers");
    capture_frame();
    if (real_glXSwapBuffers) {
        real_glXSwapBuffers(dpy, drw);
    }
}

// Our hooked eglSwapBuffers — modern EGL path (libepoxy, Wayland, headless).
unsigned int eglSwapBuffers(void *dpy, void *surface) {
    log_first_hook("eglSwapBuffers");
    capture_frame();
    if (real_eglSwapBuffers) {
        return real_eglSwapBuffers(dpy, surface);
    }
    return 1; // EGL_TRUE fallback; shouldn't happen if the real symbol resolves
}

// Our hooked SDL_GL_SwapWindow — catches SDL2's abstraction layer if xemu
// calls it directly (some builds do this instead of routing through GL/EGL).
int SDL_GL_SwapWindow(void *window) {
    log_first_hook("SDL_GL_SwapWindow");
    capture_frame();
    if (real_SDL_GL_SwapWindow) {
        return real_SDL_GL_SwapWindow(window);
    }
    return 0;
}

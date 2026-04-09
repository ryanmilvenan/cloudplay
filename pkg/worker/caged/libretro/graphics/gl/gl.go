package gl

// Custom OpenGL bindings
// Based on https://github.com/go-gl/gl/tree/master/v2.1/gl

/*
#cgo egl,windows LDFLAGS: -lEGL
#cgo egl,darwin  LDFLAGS: -lEGL
#cgo !gles2,darwin        LDFLAGS: -framework OpenGL
#cgo gles2,darwin         LDFLAGS: -lGLESv2
#cgo !gles2,windows       LDFLAGS: -lopengl32
#cgo gles2,windows        LDFLAGS: -lGLESv2
#cgo !egl,linux !egl,freebsd !egl,openbsd pkg-config: gl
#cgo egl,linux egl,freebsd egl,openbsd    pkg-config: egl

#if defined(_WIN32) && !defined(APIENTRY) && !defined(__CYGWIN__) && !defined(__SCITECH_SNAP__)
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN 1
#endif

#include <windows.h>

#endif
#ifndef APIENTRY
#define APIENTRY
#endif
#ifndef APIENTRYP
#define APIENTRYP APIENTRY*
#endif
#ifndef GLAPI
#define GLAPI extern
#endif

#include <stdio.h>
#include <KHR/khrplatform.h>

typedef unsigned int GLenum;
typedef unsigned char GLboolean;
typedef unsigned int GLbitfield;
typedef khronos_int8_t GLbyte;
typedef khronos_uint8_t GLubyte;
typedef khronos_int16_t GLshort;
typedef khronos_uint16_t GLushort;
typedef int GLint;
typedef unsigned int GLuint;
typedef khronos_int32_t GLclampx;
typedef int GLsizei;
typedef khronos_float_t GLfloat;
typedef khronos_float_t GLclampf;
typedef double GLdouble;
typedef double GLclampd;
typedef void *GLeglClientBufferEXT;
typedef void *GLeglImageOES;
typedef char GLchar;
typedef char GLcharARB;


#ifdef __APPLE__
typedef void *GLhandleARB;
#else
typedef unsigned int GLhandleARB;
#endif

typedef const GLubyte *(APIENTRYP GPGETSTRING)(GLenum name);
typedef GLenum (APIENTRYP GPCHECKFRAMEBUFFERSTATUS)(GLenum target);
typedef GLenum (APIENTRYP GPGETERROR)();
typedef void (APIENTRYP GPBINDFRAMEBUFFER)(GLenum target, GLuint framebuffer);
typedef void (APIENTRYP GPBINDRENDERBUFFER)(GLenum target, GLuint renderbuffer);
typedef void (APIENTRYP GPBINDTEXTURE)(GLenum target, GLuint texture);
typedef void (APIENTRYP GPDELETEFRAMEBUFFERS)(GLsizei n, const GLuint *framebuffers);
typedef void (APIENTRYP GPDELETERENDERBUFFERS)(GLsizei n, const GLuint *renderbuffers);
typedef void (APIENTRYP GPDELETETEXTURES)(GLsizei n, const GLuint* textures);
typedef void (APIENTRYP GPFRAMEBUFFERRENDERBUFFER)(GLenum target, GLenum attachment, GLenum renderbuffertarget, GLuint renderbuffer);
typedef void (APIENTRYP GPFRAMEBUFFERTEXTURE2D)(GLenum target, GLenum attachment, GLenum textarget, GLuint texture, GLint level);
typedef void (APIENTRYP GPGENFRAMEBUFFERS)(GLsizei n, GLuint *framebuffers);
typedef void (APIENTRYP GPGENRENDERBUFFERS)(GLsizei n, GLuint *renderbuffers);
typedef void (APIENTRYP GPGENTEXTURES)(GLsizei n, GLuint *textures);
typedef void (APIENTRYP GPREADPIXELS)(GLint x, GLint y, GLsizei width, GLsizei height, GLenum format, GLenum type, void *pixels);
typedef void (APIENTRYP GPRENDERBUFFERSTORAGE)(GLenum target, GLenum internalformat, GLsizei width, GLsizei height);
typedef void (APIENTRYP GPTEXIMAGE2D)(GLenum target, GLint level, GLint internalformat, GLsizei width, GLsizei height, GLint border, GLenum format, GLenum type, const void *pixels);
typedef void (APIENTRYP GPTEXPARAMETERI)(GLenum target, GLenum pname, GLint param);
typedef void (APIENTRYP GPPIXELSTOREI)(GLenum pname, GLint param);
typedef void (APIENTRYP GPGETINTEGERV)(GLenum pname, GLint *data);
typedef void (APIENTRYP GPBLITFRAMEBUFFER)(GLint srcX0, GLint srcY0, GLint srcX1, GLint srcY1, GLint dstX0, GLint dstY0, GLint dstX1, GLint dstY1, GLbitfield mask, GLenum filter);
typedef void (APIENTRYP GPTEXSTORAGE2D)(GLenum target, GLsizei levels, GLenum internalformat, GLsizei width, GLsizei height);

// ---------------------------------------------------------------------------
// glsm-inspired FBO state tracking & FBO 0 → frontend remap
// ---------------------------------------------------------------------------

static GPBINDFRAMEBUFFER cloudplay_core_bind_framebuffer_real = NULL;
static unsigned long long cloudplay_core_bind_framebuffer_n = 0;
static GLuint cloudplay_core_last_draw_fbo = 0;
static GLuint cloudplay_core_last_read_fbo = 0;
static GLuint cloudplay_core_last_private_draw_fbo = 0;
static GPBLITFRAMEBUFFER cloudplay_core_blit_framebuffer_real = NULL;
static unsigned long long cloudplay_core_blit_framebuffer_n = 0;

// FBO 0 → frontend remap (the core glsm trick)
static GLuint cloudplay_frontend_fbo = 0;
static int    cloudplay_fbo0_remap   = 0;  // 0=off, 1=on

// Attachment tracking: up to 32 FBOs with COLOR_ATTACHMENT0 info
#define CLOUDPLAY_MAX_TRACKED_FBOS 32
struct cloudplay_fbo_info {
  GLuint id;
  GLuint color0_tex;       // texture attached to COLOR_ATTACHMENT0 (0 = none)
  GLenum color0_textarget; // e.g. GL_TEXTURE_2D
  GLint  color0_level;
  GLuint color0_rbo;       // renderbuffer attached to COLOR_ATTACHMENT0 (0 = none)
  GLuint depth_rbo;        // renderbuffer attached to DEPTH_ATTACHMENT or DEPTH_STENCIL
};
static struct cloudplay_fbo_info cloudplay_fbo_table[CLOUDPLAY_MAX_TRACKED_FBOS];
static int cloudplay_fbo_table_len = 0;

// Texture metadata tracking: intercept glTexImage2D/glTexStorage2D
#define CLOUDPLAY_MAX_TRACKED_TEXTURES 64
struct cloudplay_tex_info {
  GLuint id;
  GLsizei width;
  GLsizei height;
  GLint internalformat;
  int source; // 1 = glTexImage2D, 2 = glTexStorage2D
};
static struct cloudplay_tex_info cloudplay_tex_table[CLOUDPLAY_MAX_TRACKED_TEXTURES];
static int cloudplay_tex_table_len = 0;
static GLuint cloudplay_current_bound_tex2d = 0;

// Real function pointers for texture interception
static GPBINDTEXTURE  cloudplay_core_bind_texture_real = NULL;
static GPTEXIMAGE2D   cloudplay_core_tex_image_2d_real = NULL;
static GPTEXSTORAGE2D cloudplay_core_tex_storage_2d_real = NULL;
static GPGETINTEGERV  cloudplay_core_getintegerv_real  = NULL;

static GPFRAMEBUFFERTEXTURE2D    cloudplay_core_fb_tex2d_real = NULL;
static GPFRAMEBUFFERRENDERBUFFER cloudplay_core_fb_rbo_real   = NULL;

// Find or create a slot for the given FBO id. Returns pointer or NULL if full.
static struct cloudplay_fbo_info* cloudplay_fbo_slot(GLuint fbo_id) {
  int i;
  for (i = 0; i < cloudplay_fbo_table_len; i++) {
    if (cloudplay_fbo_table[i].id == fbo_id) return &cloudplay_fbo_table[i];
  }
  if (cloudplay_fbo_table_len < CLOUDPLAY_MAX_TRACKED_FBOS) {
    struct cloudplay_fbo_info *s = &cloudplay_fbo_table[cloudplay_fbo_table_len++];
    s->id = fbo_id;
    s->color0_tex = 0; s->color0_textarget = 0; s->color0_level = 0;
    s->color0_rbo = 0; s->depth_rbo = 0;
    return s;
  }
  return NULL;
}

// Find a tracked texture by id. Returns pointer or NULL if not found.
static struct cloudplay_tex_info* cloudplay_tex_lookup(GLuint tex_id) {
  int i;
  for (i = 0; i < cloudplay_tex_table_len; i++) {
    if (cloudplay_tex_table[i].id == tex_id) return &cloudplay_tex_table[i];
  }
  return NULL;
}

// Find or create a slot for the given texture id.
static struct cloudplay_tex_info* cloudplay_tex_slot(GLuint tex_id) {
  struct cloudplay_tex_info *t = cloudplay_tex_lookup(tex_id);
  if (t) return t;
  if (cloudplay_tex_table_len < CLOUDPLAY_MAX_TRACKED_TEXTURES) {
    t = &cloudplay_tex_table[cloudplay_tex_table_len++];
    t->id = tex_id;
    t->width = 0; t->height = 0; t->internalformat = 0; t->source = 0;
    return t;
  }
  return NULL;
}

static void cloudplay_set_bind_framebuffer_real(void *p) {
  cloudplay_core_bind_framebuffer_real = (GPBINDFRAMEBUFFER)p;
}
static void cloudplay_set_blit_framebuffer_real(void *p) {
  cloudplay_core_blit_framebuffer_real = (GPBLITFRAMEBUFFER)p;
}
static void cloudplay_set_fb_tex2d_real(void *p) {
  cloudplay_core_fb_tex2d_real = (GPFRAMEBUFFERTEXTURE2D)p;
}
static void cloudplay_set_fb_rbo_real(void *p) {
  cloudplay_core_fb_rbo_real = (GPFRAMEBUFFERRENDERBUFFER)p;
}

static void cloudplay_set_frontend_fbo(GLuint fbo) {
  cloudplay_frontend_fbo = fbo;
  fprintf(stderr, "[glsm] frontend FBO set to %u\n", (unsigned int)fbo);
}
static void cloudplay_set_fbo0_remap(int on) {
  cloudplay_fbo0_remap = on;
  fprintf(stderr, "[glsm] FBO 0 remap %s (frontend FBO=%u)\n",
    on ? "ENABLED" : "disabled", (unsigned int)cloudplay_frontend_fbo);
}

// Resolve FBO id: if remap is on and fbo==0, return frontend FBO.
static GLuint cloudplay_resolve_fbo(GLuint framebuffer) {
  if (cloudplay_fbo0_remap && framebuffer == 0 && cloudplay_frontend_fbo != 0) {
    return cloudplay_frontend_fbo;
  }
  return framebuffer;
}

static void APIENTRY cloudplay_bind_framebuffer_wrapper(GLenum target, GLuint framebuffer) {
  GLuint resolved;
  cloudplay_core_bind_framebuffer_n++;
  resolved = cloudplay_resolve_fbo(framebuffer);

  // Track logical (pre-remap) state
  if (target == 0x8D40 || target == 0x8CA9) { // FRAMEBUFFER or DRAW_FRAMEBUFFER
    cloudplay_core_last_draw_fbo = framebuffer;
    if (framebuffer != 0 && framebuffer != cloudplay_frontend_fbo) {
      cloudplay_core_last_private_draw_fbo = framebuffer;
    }
  }
  if (target == 0x8D40 || target == 0x8CA8) { // FRAMEBUFFER or READ_FRAMEBUFFER
    cloudplay_core_last_read_fbo = framebuffer;
  }

  if (cloudplay_core_bind_framebuffer_n <= 40 || (framebuffer != 0 && cloudplay_core_bind_framebuffer_n <= 200)) {
    fprintf(stderr, "[DIAG core glBindFramebuffer] n=%llu target=0x%X fbo=%u resolved=%u\n",
      cloudplay_core_bind_framebuffer_n,
      (unsigned int)target,
      (unsigned int)framebuffer,
      (unsigned int)resolved);
  }

  if (cloudplay_core_bind_framebuffer_real) {
    cloudplay_core_bind_framebuffer_real(target, resolved);
  }
}

static void* cloudplay_bind_framebuffer_wrapper_addr(void) {
  return (void*)cloudplay_bind_framebuffer_wrapper;
}

// glFramebufferTexture2D wrapper — track COLOR_ATTACHMENT0 texture per FBO
static unsigned long long cloudplay_fb_tex2d_n = 0;
static void APIENTRY cloudplay_fb_tex2d_wrapper(GLenum target, GLenum attachment, GLenum textarget, GLuint texture, GLint level) {
  GLuint cur_fbo;
  cloudplay_fb_tex2d_n++;
  cur_fbo = (target == 0x8CA8) ? cloudplay_core_last_read_fbo : cloudplay_core_last_draw_fbo;
  if (attachment == 0x8CE0) { // COLOR_ATTACHMENT0
    struct cloudplay_fbo_info *s = cloudplay_fbo_slot(cur_fbo);
    if (s) {
      s->color0_tex = texture;
      s->color0_textarget = textarget;
      s->color0_level = level;
      s->color0_rbo = 0; // texture replaces renderbuffer
    }
  }
  if (cloudplay_fb_tex2d_n <= 60) {
    fprintf(stderr, "[DIAG core glFramebufferTexture2D] n=%llu target=0x%X attach=0x%X textarget=0x%X tex=%u level=%d curFbo=%u\n",
      cloudplay_fb_tex2d_n, (unsigned int)target, (unsigned int)attachment,
      (unsigned int)textarget, (unsigned int)texture, (int)level, (unsigned int)cur_fbo);
  }
  if (cloudplay_core_fb_tex2d_real) {
    cloudplay_core_fb_tex2d_real(target, attachment, textarget, texture, level);
  }
}
static void* cloudplay_fb_tex2d_wrapper_addr(void) {
  return (void*)cloudplay_fb_tex2d_wrapper;
}

// glFramebufferRenderbuffer wrapper — track depth/color RBO per FBO
static unsigned long long cloudplay_fb_rbo_n = 0;
static void APIENTRY cloudplay_fb_rbo_wrapper(GLenum target, GLenum attachment, GLenum rbtarget, GLuint renderbuffer) {
  GLuint cur_fbo;
  struct cloudplay_fbo_info *s;
  cloudplay_fb_rbo_n++;
  cur_fbo = (target == 0x8CA8) ? cloudplay_core_last_read_fbo : cloudplay_core_last_draw_fbo;
  s = cloudplay_fbo_slot(cur_fbo);
  if (s) {
    if (attachment == 0x8CE0) { // COLOR_ATTACHMENT0
      s->color0_rbo = renderbuffer;
      s->color0_tex = 0; // renderbuffer replaces texture
    } else if (attachment == 0x8D00 || attachment == 0x821A) { // DEPTH or DEPTH_STENCIL
      s->depth_rbo = renderbuffer;
    }
  }
  if (cloudplay_fb_rbo_n <= 60) {
    fprintf(stderr, "[DIAG core glFramebufferRenderbuffer] n=%llu target=0x%X attach=0x%X rbo=%u curFbo=%u\n",
      cloudplay_fb_rbo_n, (unsigned int)target, (unsigned int)attachment,
      (unsigned int)renderbuffer, (unsigned int)cur_fbo);
  }
  if (cloudplay_core_fb_rbo_real) {
    cloudplay_core_fb_rbo_real(target, attachment, rbtarget, renderbuffer);
  }
}
static void* cloudplay_fb_rbo_wrapper_addr(void) {
  return (void*)cloudplay_fb_rbo_wrapper;
}

static void APIENTRY cloudplay_blit_framebuffer_wrapper(GLint srcX0, GLint srcY0, GLint srcX1, GLint srcY1, GLint dstX0, GLint dstY0, GLint dstX1, GLint dstY1, GLbitfield mask, GLenum filter) {
  cloudplay_core_blit_framebuffer_n++;
  if (cloudplay_core_blit_framebuffer_n <= 60) {
    fprintf(stderr, "[DIAG core glBlitFramebuffer] n=%llu readFbo=%u drawFbo=%u src=(%d,%d)-(%d,%d) dst=(%d,%d)-(%d,%d) mask=0x%X filter=0x%X\n",
      cloudplay_core_blit_framebuffer_n,
      (unsigned int)cloudplay_core_last_read_fbo,
      (unsigned int)cloudplay_core_last_draw_fbo,
      srcX0, srcY0, srcX1, srcY1,
      dstX0, dstY0, dstX1, dstY1,
      (unsigned int)mask,
      (unsigned int)filter);
  }
  if (cloudplay_core_blit_framebuffer_real) {
    cloudplay_core_blit_framebuffer_real(srcX0, srcY0, srcX1, srcY1, dstX0, dstY0, dstX1, dstY1, mask, filter);
  }
}

static void* cloudplay_blit_framebuffer_wrapper_addr(void) {
  return (void*)cloudplay_blit_framebuffer_wrapper;
}

// glBindTexture wrapper — track currently bound texture for metadata capture
static unsigned long long cloudplay_bind_texture_n = 0;
static void APIENTRY cloudplay_bind_texture_wrapper(GLenum target, GLuint texture) {
  cloudplay_bind_texture_n++;
  if (target == 0x0DE1) { // GL_TEXTURE_2D
    cloudplay_current_bound_tex2d = texture;
  }
  if (cloudplay_bind_texture_n <= 20) {
    fprintf(stderr, "[DIAG core glBindTexture] n=%llu target=0x%X tex=%u\n",
      cloudplay_bind_texture_n, (unsigned int)target, (unsigned int)texture);
  }
  if (cloudplay_core_bind_texture_real) {
    cloudplay_core_bind_texture_real(target, texture);
  }
}
static void* cloudplay_bind_texture_wrapper_addr(void) {
  return (void*)cloudplay_bind_texture_wrapper;
}
static void cloudplay_set_bind_texture_real(void *p) {
  cloudplay_core_bind_texture_real = (GPBINDTEXTURE)p;
}
static void cloudplay_set_getintegerv_real(void *p) {
  cloudplay_core_getintegerv_real = (GPGETINTEGERV)p;
}

// Query actual GL state for the currently bound GL_TEXTURE_2D.
// Used as fallback when the wrapper-tracked value is 0 (core may have
// called glBindTexture via a directly-linked GL 1.1 symbol, bypassing
// our proc-address wrapper).
static GLuint cloudplay_query_bound_tex2d(void) {
  GLint tex = 0;
  if (cloudplay_core_getintegerv_real) {
    cloudplay_core_getintegerv_real(0x8069, &tex); // GL_TEXTURE_BINDING_2D
  }
  return (GLuint)tex;
}

// glTexImage2D wrapper — capture texture metadata
static unsigned long long cloudplay_tex_image_2d_n = 0;
static void APIENTRY cloudplay_tex_image_2d_wrapper(GLenum target, GLint level, GLint internalformat, GLsizei width, GLsizei height, GLint border, GLenum format, GLenum type, const void *pixels) {
  GLuint effective_tex;
  cloudplay_tex_image_2d_n++;
  // Use wrapper-tracked value; fall back to querying real GL state
  // (core may call glBindTexture via directly-linked GL 1.1 symbol)
  effective_tex = cloudplay_current_bound_tex2d;
  if (effective_tex == 0) {
    effective_tex = cloudplay_query_bound_tex2d();
  }
  if (target == 0x0DE1 && level == 0 && effective_tex != 0) {
    struct cloudplay_tex_info *t = cloudplay_tex_slot(effective_tex);
    if (t) {
      t->width = width;
      t->height = height;
      t->internalformat = internalformat;
      t->source = 1;
    }
  }
  if (cloudplay_tex_image_2d_n <= 30) {
    fprintf(stderr, "[DIAG core glTexImage2D] n=%llu target=0x%X level=%d ifmt=0x%X %dx%d boundTex=%u effectiveTex=%u\n",
      cloudplay_tex_image_2d_n, (unsigned int)target, (int)level, (int)internalformat,
      (int)width, (int)height, (unsigned int)cloudplay_current_bound_tex2d, (unsigned int)effective_tex);
  }
  if (cloudplay_core_tex_image_2d_real) {
    cloudplay_core_tex_image_2d_real(target, level, internalformat, width, height, border, format, type, pixels);
  }
}
static void* cloudplay_tex_image_2d_wrapper_addr(void) {
  return (void*)cloudplay_tex_image_2d_wrapper;
}
static void cloudplay_set_tex_image_2d_real(void *p) {
  cloudplay_core_tex_image_2d_real = (GPTEXIMAGE2D)p;
}

// glTexStorage2D wrapper — capture texture metadata
static unsigned long long cloudplay_tex_storage_2d_n = 0;
static void APIENTRY cloudplay_tex_storage_2d_wrapper(GLenum target, GLsizei levels, GLenum internalformat, GLsizei width, GLsizei height) {
  GLuint effective_tex;
  cloudplay_tex_storage_2d_n++;
  // Use wrapper-tracked value; fall back to querying real GL state
  effective_tex = cloudplay_current_bound_tex2d;
  if (effective_tex == 0) {
    effective_tex = cloudplay_query_bound_tex2d();
  }
  if (target == 0x0DE1 && effective_tex != 0) {
    struct cloudplay_tex_info *t = cloudplay_tex_slot(effective_tex);
    if (t) {
      t->width = width;
      t->height = height;
      t->internalformat = (GLint)internalformat;
      t->source = 2;
    }
  }
  if (cloudplay_tex_storage_2d_n <= 30) {
    fprintf(stderr, "[DIAG core glTexStorage2D] n=%llu target=0x%X levels=%d ifmt=0x%X %dx%d boundTex=%u effectiveTex=%u\n",
      cloudplay_tex_storage_2d_n, (unsigned int)target, (int)levels, (unsigned int)internalformat,
      (int)width, (int)height, (unsigned int)cloudplay_current_bound_tex2d, (unsigned int)effective_tex);
  }
  if (cloudplay_core_tex_storage_2d_real) {
    cloudplay_core_tex_storage_2d_real(target, levels, internalformat, width, height);
  }
}
static void* cloudplay_tex_storage_2d_wrapper_addr(void) {
  return (void*)cloudplay_tex_storage_2d_wrapper;
}
static void cloudplay_set_tex_storage_2d_real(void *p) {
  cloudplay_core_tex_storage_2d_real = (GPTEXSTORAGE2D)p;
}

static GLuint cloudplay_core_last_private_draw_fbo_get(void) {
  return cloudplay_core_last_private_draw_fbo;
}

// ---------------------------------------------------------------------------
// Blit from core's private FBO to frontend FBO using real (unwrapped) GL calls.
// Returns 0 on success, -1 if function pointers missing, -2 if srcFbo==0.
// ---------------------------------------------------------------------------
static unsigned long long cloudplay_frontend_blit_n = 0;
static int cloudplay_blit_core_to_frontend(GLuint srcFbo, GLuint dstFbo, GLint w, GLint h) {
  if (!cloudplay_core_bind_framebuffer_real || !cloudplay_core_blit_framebuffer_real) {
    return -1;
  }
  if (srcFbo == 0) {
    return -2;
  }
  cloudplay_frontend_blit_n++;
  // Bind src as READ_FRAMEBUFFER, dst as DRAW_FRAMEBUFFER using real (unwrapped) bind
  cloudplay_core_bind_framebuffer_real(0x8CA8, srcFbo);  // GL_READ_FRAMEBUFFER
  cloudplay_core_bind_framebuffer_real(0x8CA9, dstFbo);  // GL_DRAW_FRAMEBUFFER
  cloudplay_core_blit_framebuffer_real(0, 0, w, h, 0, 0, w, h,
    0x00004000,  // GL_COLOR_BUFFER_BIT
    0x2600);     // GL_NEAREST (pixel-exact)
  // Bind frontend as FRAMEBUFFER for subsequent ReadPixels
  cloudplay_core_bind_framebuffer_real(0x8D40, dstFbo);  // GL_FRAMEBUFFER
  if (cloudplay_frontend_blit_n <= 10 || cloudplay_frontend_blit_n % 300 == 0) {
    fprintf(stderr, "[DIAG blitCoreToFrontend] n=%llu src=%u dst=%u %dx%d\n",
      cloudplay_frontend_blit_n, (unsigned int)srcFbo, (unsigned int)dstFbo, (int)w, (int)h);
  }
  return 0;
}

// Get the tracked COLOR_ATTACHMENT0 texture for a given FBO id.
// Returns 0 if not tracked or no texture attached.
static GLuint cloudplay_get_fbo_color0_tex(GLuint fbo_id) {
  int i;
  for (i = 0; i < cloudplay_fbo_table_len; i++) {
    if (cloudplay_fbo_table[i].id == fbo_id) return cloudplay_fbo_table[i].color0_tex;
  }
  return 0;
}

// Texture-attach readback: attach a core texture to the frontend FBO's
// COLOR_ATTACHMENT0, replacing its own texture temporarily.
// Returns 0 on success, negative on error.
static unsigned long long cloudplay_texattach_n = 0;
static int cloudplay_readback_via_tex_attach(GLuint coreTex, GLuint dstFbo, GLuint origTex) {
  GLenum status;
  if (!coreTex || !dstFbo) return -1;
  if (!cloudplay_core_fb_tex2d_real || !cloudplay_core_bind_framebuffer_real) return -2;
  cloudplay_texattach_n++;
  // Bind frontend FBO
  cloudplay_core_bind_framebuffer_real(0x8D40, dstFbo);  // GL_FRAMEBUFFER
  // Attach core's texture as COLOR_ATTACHMENT0
  cloudplay_core_fb_tex2d_real(0x8D40, 0x8CE0, 0x0DE1, coreTex, 0);
  // Check completeness
  status = 0;
  if (cloudplay_texattach_n <= 10 || cloudplay_texattach_n % 300 == 0) {
    fprintf(stderr, "[DIAG texAttachReadback] n=%llu coreTex=%u dstFbo=%u origTex=%u\n",
      cloudplay_texattach_n, (unsigned int)coreTex, (unsigned int)dstFbo, (unsigned int)origTex);
  }
  return 0;
}

// Restore the frontend FBO's original texture after tex-attach readback.
static void cloudplay_restore_frontend_tex(GLuint dstFbo, GLuint origTex) {
  if (!cloudplay_core_fb_tex2d_real || !cloudplay_core_bind_framebuffer_real) return;
  cloudplay_core_bind_framebuffer_real(0x8D40, dstFbo);  // GL_FRAMEBUFFER
  cloudplay_core_fb_tex2d_real(0x8D40, 0x8CE0, 0x0DE1, origTex, 0);
}

// Dump tracked FBO table to stderr (called from Go side for diagnostics)
static void cloudplay_dump_fbo_table(void) {
  int i;
  fprintf(stderr, "[glsm] FBO table (%d entries, remap=%d frontend=%u):\n",
    cloudplay_fbo_table_len, cloudplay_fbo0_remap, (unsigned int)cloudplay_frontend_fbo);
  for (i = 0; i < cloudplay_fbo_table_len; i++) {
    struct cloudplay_fbo_info *s = &cloudplay_fbo_table[i];
    fprintf(stderr, "  [%d] fbo=%u color0_tex=%u color0_rbo=%u depth_rbo=%u\n",
      i, (unsigned int)s->id, (unsigned int)s->color0_tex,
      (unsigned int)s->color0_rbo, (unsigned int)s->depth_rbo);
  }
}

// Dump tracked texture metadata table to stderr
static void cloudplay_dump_tex_table(void) {
  int i;
  fprintf(stderr, "[glsm] Texture table (%d entries, current_bound_tex2d=%u):\n",
    cloudplay_tex_table_len, (unsigned int)cloudplay_current_bound_tex2d);
  for (i = 0; i < cloudplay_tex_table_len; i++) {
    struct cloudplay_tex_info *t = &cloudplay_tex_table[i];
    fprintf(stderr, "  [%d] tex=%u %dx%d ifmt=0x%X src=%d\n",
      i, (unsigned int)t->id, (int)t->width, (int)t->height, (int)t->internalformat, t->source);
  }
}

// Candidate scanning: find all non-frontend FBOs with a color0_tex
struct cloudplay_candidate {
  GLuint fbo_id;
  GLuint tex_id;
  GLsizei tex_width;
  GLsizei tex_height;
  GLint tex_internalformat;
};
#define CLOUDPLAY_MAX_CANDIDATES 8
static int cloudplay_get_candidates(struct cloudplay_candidate *out, int max_out) {
  int count = 0;
  int i;
  for (i = 0; i < cloudplay_fbo_table_len && count < max_out; i++) {
    struct cloudplay_fbo_info *f = &cloudplay_fbo_table[i];
    struct cloudplay_tex_info *t;
    if (f->color0_tex == 0) continue;
    if (f->id == cloudplay_frontend_fbo) continue;
    out[count].fbo_id = f->id;
    out[count].tex_id = f->color0_tex;
    t = cloudplay_tex_lookup(f->color0_tex);
    if (t) {
      out[count].tex_width = t->width;
      out[count].tex_height = t->height;
      out[count].tex_internalformat = t->internalformat;
    } else {
      out[count].tex_width = 0;
      out[count].tex_height = 0;
      out[count].tex_internalformat = 0;
    }
    count++;
  }
  return count;
}

static const GLubyte *getString(GPGETSTRING ptr, GLenum name) { return (*ptr)(name); }
static GLenum getError(GPGETERROR ptr) { return (*ptr)(); }
static void bindTexture(GPBINDTEXTURE ptr, GLenum target, GLuint texture) { (*ptr)(target, texture); }
static void bindFramebuffer(GPBINDFRAMEBUFFER ptr, GLenum target, GLuint framebuffer) { (*ptr)(target, framebuffer); }
static void bindRenderbuffer(GPBINDRENDERBUFFER ptr, GLenum target, GLuint buf) { (*ptr)(target, buf); }
static void texParameteri(GPTEXPARAMETERI ptr, GLenum target, GLenum pname, GLint param) {
  (*ptr)(target, pname, param);
}
static void texImage2D(GPTEXIMAGE2D ptr, GLenum target, GLint level, GLint internalformat, GLsizei width, GLsizei height, GLint border, GLenum format, GLenum type, const void *pixels) {
  (*ptr)(target, level, internalformat, width, height, border, format, type, pixels);
}
static void genFramebuffers(GPGENFRAMEBUFFERS ptr, GLsizei n, GLuint *framebuffers) { (*ptr)(n, framebuffers); }
static void genTextures(GPGENTEXTURES ptr, GLsizei n, GLuint *textures) { (*ptr)(n, textures); }
static void framebufferTexture2D(GPFRAMEBUFFERTEXTURE2D ptr, GLenum target, GLenum attachment, GLenum textarget, GLuint texture, GLint level) {
  (*ptr)(target, attachment, textarget, texture, level);
}
static void genRenderbuffers(GPGENRENDERBUFFERS ptr, GLsizei n, GLuint *renderbuffers) { (*ptr)(n, renderbuffers); }
static void renderbufferStorage(GPRENDERBUFFERSTORAGE ptr, GLenum target, GLenum internalformat, GLsizei width, GLsizei height) {
  (*ptr)(target, internalformat, width, height);
}
static void framebufferRenderbuffer(GPFRAMEBUFFERRENDERBUFFER ptr, GLenum target, GLenum attachment, GLenum renderbuffertarget, GLuint renderbuffer) {
  (*ptr)(target, attachment, renderbuffertarget, renderbuffer);
}
static GLenum checkFramebufferStatus(GPCHECKFRAMEBUFFERSTATUS ptr, GLenum target) { return (*ptr)(target); }
static void deleteRenderbuffers(GPDELETERENDERBUFFERS ptr, GLsizei n, const GLuint *renderbuffers) {
  (*ptr)(n, renderbuffers);
}
static void deleteFramebuffers(GPDELETEFRAMEBUFFERS ptr, GLsizei n, const GLuint *framebuffers) {
  (*ptr)(n, framebuffers);
}
static void deleteTextures(GPDELETETEXTURES ptr, GLsizei n, const GLuint *textures) { (*ptr)(n, textures); }
static void readPixels(GPREADPIXELS ptr, GLint x, GLint y, GLsizei width, GLsizei height, GLenum format, GLenum type, void *pixels) {
  (*ptr)(x, y, width, height, format, type, pixels);
}
static void pixelStorei(GPPIXELSTOREI ptr, GLenum pname, GLint param) { (*ptr)(pname, param); }
static void getIntegerv(GPGETINTEGERV ptr, GLenum pname, GLint *data) { (*ptr)(pname, data); }
*/
import "C"
import (
	"errors"
	"strings"
	"unsafe"
)

const (
	VENDOR                 = 0x1F00
	VERSION                = 0x1F02
	RENDERER               = 0x1F01
	ShadingLanguageVersion = 0x8B8C
	Texture2d              = 0x0DE1
	RENDERBUFFER           = 0x8D41
	FRAMEBUFFER            = 0x8D40
	TextureMinFilter       = 0x2801
	TextureMagFilter       = 0x2800
	NEAREST                = 0x2600
	RGBA8                  = 0x8058
	BGRA                   = 0x80E1
	RGB                    = 0x1907
	ColorAttachment0       = 0x8CE0
	Depth24Stencil8        = 0x88F0
	DepthStencilAttachment = 0x821A
	DepthComponent24       = 0x81A6
	DepthAttachment        = 0x8D00
	FramebufferComplete    = 0x8CD5
	FramebufferBinding     = 0x8CA6
	ReadFramebufferBinding = 0x8CAA

	UnsignedShort5551  = 0x8034
	UnsignedShort565   = 0x8363
	UnsignedInt8888Rev = 0x8367

	PackAlignment = 0x0D05
)

var (
	gpGetString               C.GPGETSTRING
	gpGenTextures             C.GPGENTEXTURES
	gpGetError                C.GPGETERROR
	gpBindTexture             C.GPBINDTEXTURE
	gpBindFramebuffer         C.GPBINDFRAMEBUFFER
	gpTexParameteri           C.GPTEXPARAMETERI
	gpTexImage2D              C.GPTEXIMAGE2D
	gpGenFramebuffers         C.GPGENFRAMEBUFFERS
	gpFramebufferTexture2D    C.GPFRAMEBUFFERTEXTURE2D
	gpGenRenderbuffers        C.GPGENRENDERBUFFERS
	gpBindRenderbuffer        C.GPBINDRENDERBUFFER
	gpRenderbufferStorage     C.GPRENDERBUFFERSTORAGE
	gpFramebufferRenderbuffer C.GPFRAMEBUFFERRENDERBUFFER
	gpCheckFramebufferStatus  C.GPCHECKFRAMEBUFFERSTATUS
	gpDeleteRenderbuffers     C.GPDELETERENDERBUFFERS
	gpDeleteFramebuffers      C.GPDELETEFRAMEBUFFERS
	gpDeleteTextures          C.GPDELETETEXTURES
	gpReadPixels              C.GPREADPIXELS
	gpPixelStorei             C.GPPIXELSTOREI
	gpGetIntegerv             C.GPGETINTEGERV
)

func InitWithProcAddrFunc(getProcAddr func(name string) unsafe.Pointer) error {
	if gpGetString = (C.GPGETSTRING)(getProcAddr("glGetString")); gpGetString == nil {
		return errors.New("glGetString")
	}
	if gpGenTextures = (C.GPGENTEXTURES)(getProcAddr("glGenTextures")); gpGenTextures == nil {
		return errors.New("glGenTextures")
	}
	if gpGetError = (C.GPGETERROR)(getProcAddr("glGetError")); gpGetError == nil {
		return errors.New("glGetError")
	}
	if gpBindTexture = (C.GPBINDTEXTURE)(getProcAddr("glBindTexture")); gpBindTexture == nil {
		return errors.New("glBindTexture")
	}
	if gpBindFramebuffer = (C.GPBINDFRAMEBUFFER)(getProcAddr("glBindFramebuffer")); gpBindFramebuffer == nil {
		return errors.New("glBindFramebuffer")
	}
	if gpTexParameteri = (C.GPTEXPARAMETERI)(getProcAddr("glTexParameteri")); gpTexParameteri == nil {
		return errors.New("glTexParameteri")
	}
	if gpTexImage2D = (C.GPTEXIMAGE2D)(getProcAddr("glTexImage2D")); gpTexImage2D == nil {
		return errors.New("glTexImage2D")
	}
	gpGenFramebuffers = (C.GPGENFRAMEBUFFERS)(getProcAddr("glGenFramebuffers"))
	gpFramebufferTexture2D = (C.GPFRAMEBUFFERTEXTURE2D)(getProcAddr("glFramebufferTexture2D"))
	gpGenRenderbuffers = (C.GPGENRENDERBUFFERS)(getProcAddr("glGenRenderbuffers"))
	gpBindRenderbuffer = (C.GPBINDRENDERBUFFER)(getProcAddr("glBindRenderbuffer"))
	gpRenderbufferStorage = (C.GPRENDERBUFFERSTORAGE)(getProcAddr("glRenderbufferStorage"))
	gpFramebufferRenderbuffer = (C.GPFRAMEBUFFERRENDERBUFFER)(getProcAddr("glFramebufferRenderbuffer"))
	gpCheckFramebufferStatus = (C.GPCHECKFRAMEBUFFERSTATUS)(getProcAddr("glCheckFramebufferStatus"))
	gpDeleteRenderbuffers = (C.GPDELETERENDERBUFFERS)(getProcAddr("glDeleteRenderbuffers"))
	gpDeleteFramebuffers = (C.GPDELETEFRAMEBUFFERS)(getProcAddr("glDeleteFramebuffers"))
	if gpDeleteTextures = (C.GPDELETETEXTURES)(getProcAddr("glDeleteTextures")); gpDeleteTextures == nil {
		return errors.New("glDeleteTextures")
	}
	gpReadPixels = (C.GPREADPIXELS)(getProcAddr("glReadPixels"))
	if gpReadPixels == nil {
		return errors.New("glReadPixels")
	}
	if gpPixelStorei = (C.GPPIXELSTOREI)(getProcAddr("glPixelStorei")); gpPixelStorei == nil {
		return errors.New("glPixelStorei")
	}
	if gpGetIntegerv = (C.GPGETINTEGERV)(getProcAddr("glGetIntegerv")); gpGetIntegerv == nil {
		return errors.New("glGetIntegerv")
	}
	// Give the C-side texture wrappers access to glGetIntegerv so they can
	// query GL_TEXTURE_BINDING_2D as a fallback when the wrapper-tracked
	// bound texture is 0 (core may call glBindTexture via directly-linked
	// GL 1.1 symbol, bypassing our proc-address wrapper).
	C.cloudplay_set_getintegerv_real(unsafe.Pointer(gpGetIntegerv))
	return nil
}

func GetString(name uint32) *uint8 { return (*uint8)(C.getString(gpGetString, (C.GLenum)(name))) }
func GenTextures(n int32, textures *uint32) {
	C.genTextures(gpGenTextures, (C.GLsizei)(n), (*C.GLuint)(unsafe.Pointer(textures)))
}
func BindTexture(target uint32, texture uint32) {
	C.bindTexture(gpBindTexture, (C.GLenum)(target), (C.GLuint)(texture))
}
func BindFramebuffer(target uint32, framebuffer uint32) {
	C.bindFramebuffer(gpBindFramebuffer, (C.GLenum)(target), (C.GLuint)(framebuffer))
}
func TexParameteri(target uint32, name uint32, param int32) {
	C.texParameteri(gpTexParameteri, (C.GLenum)(target), (C.GLenum)(name), (C.GLint)(param))
}
func TexImage2D(target uint32, level int32, internalformat int32, width int32, height int32, border int32, format uint32, xtype uint32, pixels unsafe.Pointer) {
	C.texImage2D(gpTexImage2D, (C.GLenum)(target), (C.GLint)(level), (C.GLint)(internalformat), (C.GLsizei)(width), (C.GLsizei)(height), (C.GLint)(border), (C.GLenum)(format), (C.GLenum)(xtype), pixels)
}
func GenFramebuffers(n int32, framebuffers *uint32) {
	C.genFramebuffers(gpGenFramebuffers, (C.GLsizei)(n), (*C.GLuint)(unsafe.Pointer(framebuffers)))
}
func FramebufferTexture2D(target uint32, attachment uint32, texTarget uint32, texture uint32, level int32) {
	C.framebufferTexture2D(gpFramebufferTexture2D, (C.GLenum)(target), (C.GLenum)(attachment), (C.GLenum)(texTarget), (C.GLuint)(texture), (C.GLint)(level))
}
func GenRenderbuffers(n int32, renderbuffers *uint32) {
	C.genRenderbuffers(gpGenRenderbuffers, (C.GLsizei)(n), (*C.GLuint)(unsafe.Pointer(renderbuffers)))
}
func BindRenderbuffer(target uint32, renderbuffer uint32) {
	C.bindRenderbuffer(gpBindRenderbuffer, (C.GLenum)(target), (C.GLuint)(renderbuffer))
}
func RenderbufferStorage(target uint32, internalformat uint32, width int32, height int32) {
	C.renderbufferStorage(gpRenderbufferStorage, (C.GLenum)(target), (C.GLenum)(internalformat), (C.GLsizei)(width), (C.GLsizei)(height))
}
func FramebufferRenderbuffer(target uint32, attachment uint32, renderbufferTarget uint32, renderbuffer uint32) {
	C.framebufferRenderbuffer(gpFramebufferRenderbuffer, (C.GLenum)(target), (C.GLenum)(attachment), (C.GLenum)(renderbufferTarget), (C.GLuint)(renderbuffer))
}
func CheckFramebufferStatus(target uint32) uint32 {
	return (uint32)(C.checkFramebufferStatus(gpCheckFramebufferStatus, (C.GLenum)(target)))
}
func DeleteRenderbuffers(n int32, renderbuffers *uint32) {
	C.deleteRenderbuffers(gpDeleteRenderbuffers, (C.GLsizei)(n), (*C.GLuint)(unsafe.Pointer(renderbuffers)))
}
func DeleteFramebuffers(n int32, framebuffers *uint32) {
	C.deleteFramebuffers(gpDeleteFramebuffers, (C.GLsizei)(n), (*C.GLuint)(unsafe.Pointer(framebuffers)))
}
func DeleteTextures(n int32, textures *uint32) {
	C.deleteTextures(gpDeleteTextures, (C.GLsizei)(n), (*C.GLuint)(unsafe.Pointer(textures)))
}
func ReadPixels(x int32, y int32, width int32, height int32, format uint32, xtype uint32, pixels unsafe.Pointer) {
	C.readPixels(gpReadPixels, (C.GLint)(x), (C.GLint)(y), (C.GLsizei)(width), (C.GLsizei)(height), (C.GLenum)(format), (C.GLenum)(xtype), pixels)
}
func PixelStorei(pname uint32, param int32) {
	C.pixelStorei(gpPixelStorei, (C.GLenum)(pname), (C.GLint)(param))
}
func GetIntegerv(pname uint32, data *int32) {
	C.getIntegerv(gpGetIntegerv, (C.GLenum)(pname), (*C.GLint)(unsafe.Pointer(data)))
}

func GetError() uint32 { return (uint32)(C.getError(gpGetError)) }

func GoStr(str *uint8) string { return C.GoString((*C.char)(unsafe.Pointer(str))) }

func CoreLastPrivateDrawFbo() int32 {
	return int32(C.cloudplay_core_last_private_draw_fbo_get())
}

// CoreBlitToFrontend blits from a core private FBO to the frontend FBO.
// Uses real (unwrapped) GL calls to avoid remap interference.
// Returns 0 on success.
func CoreBlitToFrontend(srcFbo, dstFbo uint32, w, h int32) int32 {
	return int32(C.cloudplay_blit_core_to_frontend(C.GLuint(srcFbo), C.GLuint(dstFbo), C.GLint(w), C.GLint(h)))
}

// CoreSetFrontendFbo tells the C wrapper layer which FBO is the frontend's
// render target. FBO 0 remapping will redirect core binds of FBO 0 here.
func CoreSetFrontendFbo(fbo uint32) {
	C.cloudplay_set_frontend_fbo(C.GLuint(fbo))
}

// CoreSetFbo0Remap enables/disables the glsm-style FBO 0 → frontend FBO remap.
func CoreSetFbo0Remap(on bool) {
	v := C.int(0)
	if on {
		v = 1
	}
	C.cloudplay_set_fbo0_remap(v)
}

// CoreDumpFboTable prints the tracked FBO attachment table to stderr.
func CoreDumpFboTable() {
	C.cloudplay_dump_fbo_table()
}

// CoreGetFboColor0Tex returns the tracked COLOR_ATTACHMENT0 texture for a given FBO.
func CoreGetFboColor0Tex(fboID uint32) uint32 {
	return uint32(C.cloudplay_get_fbo_color0_tex(C.GLuint(fboID)))
}

// CoreReadbackViaTexAttach temporarily attaches coreTex to dstFbo's COLOR_ATTACHMENT0.
// Call CoreRestoreFrontendTex after ReadPixels to restore the original texture.
func CoreReadbackViaTexAttach(coreTex, dstFbo, origTex uint32) int32 {
	return int32(C.cloudplay_readback_via_tex_attach(C.GLuint(coreTex), C.GLuint(dstFbo), C.GLuint(origTex)))
}

// CoreRestoreFrontendTex restores the frontend FBO's original texture after tex-attach readback.
func CoreRestoreFrontendTex(dstFbo, origTex uint32) {
	C.cloudplay_restore_frontend_tex(C.GLuint(dstFbo), C.GLuint(origTex))
}

// CoreDumpTexTable prints the tracked texture metadata table to stderr.
func CoreDumpTexTable() {
	C.cloudplay_dump_tex_table()
}

// Candidate represents a tracked FBO+texture pair for readback scanning.
type Candidate struct {
	FboID          uint32
	TexID          uint32
	TexWidth       int32
	TexHeight      int32
	TexInternalFmt int32
}

// CoreGetCandidates returns all non-frontend FBOs with tracked color textures.
func CoreGetCandidates() []Candidate {
	var buf [8]C.struct_cloudplay_candidate
	n := int(C.cloudplay_get_candidates(&buf[0], 8))
	result := make([]Candidate, n)
	for i := 0; i < n; i++ {
		result[i] = Candidate{
			FboID:          uint32(buf[i].fbo_id),
			TexID:          uint32(buf[i].tex_id),
			TexWidth:       int32(buf[i].tex_width),
			TexHeight:      int32(buf[i].tex_height),
			TexInternalFmt: int32(buf[i].tex_internalformat),
		}
	}
	return result
}

func CoreWrapProcAddress(name string, ptr unsafe.Pointer) unsafe.Pointer {
	if ptr == nil {
		return nil
	}
	switch {
	case name == "glBindFramebuffer" || name == "glBindFramebufferEXT":
		C.cloudplay_set_bind_framebuffer_real(ptr)
		return C.cloudplay_bind_framebuffer_wrapper_addr()
	case name == "glBlitFramebuffer" || name == "glBlitFramebufferEXT":
		C.cloudplay_set_blit_framebuffer_real(ptr)
		return C.cloudplay_blit_framebuffer_wrapper_addr()
	case name == "glFramebufferTexture2D" || name == "glFramebufferTexture2DEXT":
		C.cloudplay_set_fb_tex2d_real(ptr)
		return C.cloudplay_fb_tex2d_wrapper_addr()
	case name == "glFramebufferRenderbuffer" || name == "glFramebufferRenderbufferEXT":
		C.cloudplay_set_fb_rbo_real(ptr)
		return C.cloudplay_fb_rbo_wrapper_addr()
	case name == "glBindTexture":
		C.cloudplay_set_bind_texture_real(ptr)
		return C.cloudplay_bind_texture_wrapper_addr()
	case name == "glTexImage2D":
		C.cloudplay_set_tex_image_2d_real(ptr)
		return C.cloudplay_tex_image_2d_wrapper_addr()
	case name == "glTexStorage2D":
		C.cloudplay_set_tex_storage_2d_real(ptr)
		return C.cloudplay_tex_storage_2d_wrapper_addr()
	case strings.Contains(name, "Framebuffer") || strings.Contains(name, "Blit"):
		return ptr
	default:
		return ptr
	}
}

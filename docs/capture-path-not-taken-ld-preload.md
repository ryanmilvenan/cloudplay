# Capture path we did NOT take: LD_PRELOAD GL interception

This document records a video/audio capture approach we chose **not** to use
after Phase 3/4 of the xemu native-emulator work. We went with x11grab
piping instead. If x11grab ever becomes a performance bottleneck, this is
where to resume.

## The path we're not taking

Hook `SDL_GL_SwapWindow` (or `glXSwapBuffers` / `eglSwapBuffers`) via
`LD_PRELOAD`, call `glReadPixels` before forwarding to the real swap, and
ship RGBA bytes over a Unix socket to a Go receiver.

Source lived at:

- `pkg/worker/caged/xemu/preload/videocap_preload.c` — the C shim (~210 LOC)
- `pkg/worker/caged/xemu/videocap.go` — Go socket receiver (~170 LOC)
- `tools/xemu-canary/` — golden-frame validator

See those files on `feat/xemu-phase-4-audio` (commit `8a59f617`) or earlier
for the full implementation.

## Why it was attractive

- **Zero X-server round-trip.** glReadPixels on NVIDIA pulls the framebuffer
  straight from the driver's VRAM mapping. x11grab makes the X server
  composite the screen, round-trip a CopyArea, and deliver XGetImage bytes.
- **Process-scoped.** Captures only xemu's framebuffer, not whatever else
  happens to be on the display. Matters for headless production where the
  display server may host the GUI of a debugger or a second session.
- **Optional future zero-copy to NVENC.** Once frames are in a GPU-owned
  buffer via glReadPixels we can teach the shim to blit into a shared CUDA
  allocation and bypass the GPU→CPU→GPU round-trip that x11grab forces.
  cloudplay already does this trick for libretro (see
  `docs/arch-backend.md` Phase-3 Vulkan zero-copy note).
- **Backend-generic.** Works for any GL application, not just xemu.

## Why we didn't ship it (the reason to revisit: all fixable)

xemu creates **four SDL windows** per process, not one:

```
$ xwininfo -root -tree (inside Xvfb :100 with xemu running)
 2097190 "SDL Offscreen Window": ("xemu" "xemu")  640x480+192+144
 2097184 "SDL Offscreen Window": ("xemu" "xemu")  640x480+192+144
 2097178 "SDL Offscreen Window": ("xemu" "xemu")  640x480+192+144
 2097172 "xemu | v0.8.5":        ("xemu" "xemu")  640x480+192+144
```

Our shim's `SDL_GL_SwapWindow` hook fired on the first swap it saw and
tagged that as "our frame stream". Every run produced identical frame
bytes (SHA `aeebfcde644dfcffd071db476dabd5c3769c849c68a62ac768137770d2746730`),
which we believed for weeks was the xemu-rendered output. It was actually
one of the **three offscreen windows** rendering xemu's idle overlay
("Guest has not initialized the display (yet).").

Diagnostic that finally surfaced it: `ffmpeg -f x11grab -i :100` captured
the Xbox boot-logo animation → Xbox logo → dashboard, while our LD_PRELOAD
shim captured the same "Guest not initialized" frame the whole time.

## Other things we learned along the way (don't redo them)

1. **`RTLD_NEXT` doesn't resolve glReadPixels/glGetIntegerv** for xemu
   because xemu pulls GL through libepoxy, which doesn't re-export the
   low-level symbols. Use `RTLD_DEFAULT` (with an optional
   `dlopen("libGL.so.1", RTLD_NOW|RTLD_NOLOAD)` fallback) for read-side GL
   calls. Keep `RTLD_NEXT` for the swap hook itself (needs to call through
   to the real implementation).
2. **xemu routes swaps through `SDL_GL_SwapWindow`, not the lower-level
   `glXSwapBuffers`/`eglSwapBuffers`.** Hook all three anyway (defense in
   depth), log which one fires first, set one-shot `g_hook_logged`.
3. **`.c` next to `.go` trips `go build`** — "C source files not allowed
   when not using cgo or SWIG". Keep preload sources under
   `pkg/worker/caged/xemu/preload/` so the Go toolchain ignores them.
4. **x86_64 uinput ioctl numbers** that we precomputed are not
   architecture-independent — see `pkg/worker/caged/xemu/input.go` for the
   constants we wrote down. If we ever run cloudplay on ARM we'd need to
   recompute or use `unix.IoctlSetInt` style macros.

## Re-entry checklist (if we come back)

Do these in order. Each step is testable in isolation so you can bail
early if the first insight fixes the original problem.

1. **Filter the hook by SDL window pointer.** `SDL_GL_SwapWindow(SDL_Window *)`
   carries the window identity. The visible xemu window is the one whose
   `SDL_GetWindowTitle()` matches `"xemu | v*"`. Cache the pointer on
   first sight and ignore swaps on other windows. This alone is probably
   the fix — the three offscreen windows are xemu's internal FBO-bound
   contexts (shader compile, YUV, whatever).

   ```c
   static void *g_target_window = NULL;

   int SDL_GL_SwapWindow(void *window) {
       if (!g_target_window) {
           // resolve SDL_GetWindowTitle at load time; compare
           const char *title = sym_SDL_GetWindowTitle(window);
           if (title && strncmp(title, "xemu | v", 8) == 0) {
               g_target_window = window;
           }
       }
       if (window == g_target_window) {
           capture_frame();
       }
       return real_SDL_GL_SwapWindow(window);
   }
   ```

2. **Compare frame hashes between hook modes.** Run xemu with Complex_4627
   + NevolutionX disc. With the window filter applied, the first live
   frame should be the Xbox logo, not the idle overlay. If it is, Phase 3
   gates are met directly — no x11grab needed. If it still isn't, jump to
   step 3.

3. **Investigate the FBO path.** xemu may be rendering NV2A output into an
   FBO that it binds as a texture inside the "xemu | v*" window's GL
   context, then compositing that texture plus ImGui widgets into the
   default framebuffer. `glReadPixels` on the default framebuffer grabs
   the composite, which is what we want. But if xemu is using a multi-
   pass render where the final SwapBuffers happens on an FBO that then
   gets X11-blitted (via xshm or similar), our hook would miss the final
   blit.

   Diagnostic: wrap `glReadPixels` and log `GL_READ_FRAMEBUFFER_BINDING`
   before the read. If it's non-zero, we're reading from an FBO, not the
   default framebuffer. Call `glBindFramebuffer(GL_READ_FRAMEBUFFER, 0)`
   before `glReadPixels` and restore afterward.

4. **If FBO-mode is the story, look at `glBlitFramebuffer`** as an
   alternative to glReadPixels. It's generally faster for GPU-side copies.

5. **Validate with the xemu-canary harness.** The plan originally called
   for a standalone GL test program (three known RGBA patterns, SHA256
   diff against goldens). It was deferred because we believed the xemu
   validation was sufficient. Revive it if we ship this path — it
   exercises the shim without xemu's multi-window confusion.

## Rough performance envelope (why this matters)

At 640×480 RGBA 60 fps:

| Path                 | CPU read cost     | PCIe cost         | Best case latency | Parallelism |
|---|---|---|---|---|
| x11grab pipe         | 1.2 MB/frame × 60 = 72 MB/s | GPU→CPU full readback | ~3-8 ms  | Blocking on X11 |
| LD_PRELOAD glReadPixels | Same readback cost in principle, BUT avoids X server round-trip | GPU→CPU full readback | ~1-3 ms  | Parallel with render thread |
| LD_PRELOAD → CUDA shared | Zero CPU copy | Stays in VRAM | ~0.5 ms | Parallel |

x11grab is fine for 640×480 because even the worst case is ~400 MB/s and
our pipeline can absorb that. At 1080p (6 MB/frame × 60 = 360 MB/s, 4×
larger) the x11grab pipe starts to meaningfully compete with NVENC for
PCIe bandwidth. That's the trigger to return here.

## References

- `feat/xemu-phase-4-audio` commit `8a59f617` — last version of the full
  shim code, harness, and unit tests.
- `feat/xemu-phase-3-video` commit `95b83b24` — shim's initial landing.
- `pkg/worker/caged/xemu/caged.go` — video callback wiring; the
  LiveFramesActive() flag + stub emitter are reusable by either capture
  approach.

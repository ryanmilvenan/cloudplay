//go:build vulkan

package vulkan

// surface.go – headless VkSurfaceKHR creation for the core negotiation path.
//
// Dolphin (and every other Vulkan libretro core) needs a valid VkSurfaceKHR
// in create_device2 so it can create a swapchain and begin calling
// retro_video_refresh().  We try the cleanest approach first:
//
//  1. VK_EXT_headless_surface — no display server required; NVIDIA driver 418+
//     (RTX 30xx definitely supports this).
//  2. VK_KHR_xlib_surface    — Xlib surface on the Xvfb display :99 that the
//     container always starts (Dockerfile.run).  A 1×1 invisible window is
//     enough; we never actually show anything.
//
// The VkInstance MUST have been created with the matching surface extension
// (enforced by NewInstanceOnly in context.go).
//
// NOTE: We avoid #including vulkan_xlib.h directly because the build container
// may not have libx11-dev.  Instead, we dlopen libX11 and resolve Xlib
// functions at runtime (the worker runtime image already has libx11-6).
// For the headless path, no X11 dependency is needed at all.

/*
#cgo LDFLAGS: -lvulkan -ldl
#include <vulkan/vulkan.h>
#include <dlfcn.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

// ── Headless surface ──────────────────────────────────────────────────────

static VkResult cloudplay_create_headless_surface(VkInstance instance, VkSurfaceKHR *out) {
    PFN_vkCreateHeadlessSurfaceEXT fn = (PFN_vkCreateHeadlessSurfaceEXT)
        vkGetInstanceProcAddr(instance, "vkCreateHeadlessSurfaceEXT");
    if (!fn) return VK_ERROR_EXTENSION_NOT_PRESENT;

    VkHeadlessSurfaceCreateInfoEXT info;
    memset(&info, 0, sizeof(info));
    info.sType = VK_STRUCTURE_TYPE_HEADLESS_SURFACE_CREATE_INFO_EXT;
    return fn(instance, &info, NULL, out);
}

// ── Xlib surface (Xvfb fallback) ──────────────────────────────────────────
// Uses dlopen to avoid compile-time X11 header dependency.
// The runtime container has libX11.so from the libx11-6 package.

typedef void* XlibDisplay;
typedef unsigned long XlibWindow;

typedef XlibDisplay (*PFN_XOpenDisplay)(const char*);
typedef int (*PFN_XCloseDisplay)(XlibDisplay);
typedef XlibWindow (*PFN_XDefaultRootWindow)(XlibDisplay);
typedef XlibWindow (*PFN_XCreateSimpleWindow)(XlibDisplay, XlibWindow, int, int,
    unsigned int, unsigned int, unsigned int, unsigned long, unsigned long);
typedef int (*PFN_XDestroyWindow)(XlibDisplay, XlibWindow);

// VkXlibSurfaceCreateInfoKHR layout (matches vulkan_xlib.h without needing the header)
typedef struct {
    VkStructureType sType;
    const void*     pNext;
    VkFlags         flags;
    XlibDisplay     dpy;
    XlibWindow      window;
} CloudplayXlibSurfaceCreateInfo;

typedef VkResult (*PFN_vkCreateXlibSurfaceKHR_)(
    VkInstance, const CloudplayXlibSurfaceCreateInfo*, const VkAllocationCallbacks*, VkSurfaceKHR*);

static VkResult cloudplay_create_xlib_surface(
        VkInstance instance,
        VkSurfaceKHR *out_surface,
        void **out_dpy,       // opaque Display*
        unsigned long *out_win,
        void **out_libx11)    // dlopen handle for cleanup
{
    *out_dpy = NULL;
    *out_win = 0;
    *out_libx11 = NULL;

    // dlopen libX11
    void *libx11 = dlopen("libX11.so.6", RTLD_LAZY);
    if (!libx11) libx11 = dlopen("libX11.so", RTLD_LAZY);
    if (!libx11) return VK_ERROR_INITIALIZATION_FAILED;

    PFN_XOpenDisplay pXOpenDisplay = (PFN_XOpenDisplay)dlsym(libx11, "XOpenDisplay");
    PFN_XDefaultRootWindow pXDefaultRootWindow = (PFN_XDefaultRootWindow)dlsym(libx11, "XDefaultRootWindow");
    PFN_XCreateSimpleWindow pXCreateSimpleWindow = (PFN_XCreateSimpleWindow)dlsym(libx11, "XCreateSimpleWindow");
    PFN_XDestroyWindow pXDestroyWindow = (PFN_XDestroyWindow)dlsym(libx11, "XDestroyWindow");
    PFN_XCloseDisplay pXCloseDisplay = (PFN_XCloseDisplay)dlsym(libx11, "XCloseDisplay");

    if (!pXOpenDisplay || !pXDefaultRootWindow || !pXCreateSimpleWindow) {
        dlclose(libx11);
        return VK_ERROR_INITIALIZATION_FAILED;
    }

    XlibDisplay dpy = pXOpenDisplay(":99");
    if (!dpy) {
        // Try default DISPLAY
        dpy = pXOpenDisplay(NULL);
    }
    if (!dpy) {
        dlclose(libx11);
        return VK_ERROR_INITIALIZATION_FAILED;
    }

    XlibWindow root = pXDefaultRootWindow(dpy);
    XlibWindow win = pXCreateSimpleWindow(dpy, root, 0, 0, 1, 1, 0, 0, 0);

    PFN_vkCreateXlibSurfaceKHR_ vkFn = (PFN_vkCreateXlibSurfaceKHR_)
        vkGetInstanceProcAddr(instance, "vkCreateXlibSurfaceKHR");
    if (!vkFn) {
        if (pXDestroyWindow) pXDestroyWindow(dpy, win);
        if (pXCloseDisplay) pXCloseDisplay(dpy);
        dlclose(libx11);
        return VK_ERROR_EXTENSION_NOT_PRESENT;
    }

    CloudplayXlibSurfaceCreateInfo info;
    memset(&info, 0, sizeof(info));
    info.sType  = VK_STRUCTURE_TYPE_XLIB_SURFACE_CREATE_INFO_KHR;
    info.dpy    = dpy;
    info.window = win;

    VkResult res = vkFn(instance, &info, NULL, out_surface);
    if (res != VK_SUCCESS) {
        if (pXDestroyWindow) pXDestroyWindow(dpy, win);
        if (pXCloseDisplay) pXCloseDisplay(dpy);
        dlclose(libx11);
        return res;
    }
    *out_dpy = dpy;
    *out_win = win;
    *out_libx11 = libx11;
    return VK_SUCCESS;
}

// ── Cleanup ───────────────────────────────────────────────────────────────

static void cloudplay_destroy_surface(VkInstance instance, VkSurfaceKHR surface) {
    if (surface != VK_NULL_HANDLE)
        vkDestroySurfaceKHR(instance, surface, NULL);
}

static void cloudplay_close_xlib(void *dpy, unsigned long win, void *libx11) {
    if (!libx11) return;
    if (dpy) {
        PFN_XDestroyWindow pXDestroyWindow = (PFN_XDestroyWindow)dlsym(libx11, "XDestroyWindow");
        PFN_XCloseDisplay pXCloseDisplay = (PFN_XCloseDisplay)dlsym(libx11, "XCloseDisplay");
        if (pXDestroyWindow) pXDestroyWindow(dpy, win);
        if (pXCloseDisplay) pXCloseDisplay(dpy);
    }
    dlclose(libx11);
}

// ── Presentation-support query ────────────────────────────────────────────
static int cloudplay_check_surface_support(
        VkPhysicalDevice gpu,
        uint32_t         queue_family,
        VkSurfaceKHR     surface)
{
    VkBool32 supported = VK_FALSE;
    VkResult res = vkGetPhysicalDeviceSurfaceSupportKHR(gpu, queue_family, surface, &supported);
    if (res != VK_SUCCESS) return -1;
    return (int)supported;
}
*/
import "C"
import (
	"fmt"
	"unsafe"
)

// HeadlessSurface wraps a VkSurfaceKHR and any X11 resources needed for the
// xlib fallback path.  Call Destroy() before the parent VkInstance is
// destroyed.
type HeadlessSurface struct {
	surface    C.VkSurfaceKHR
	xlibDpy    unsafe.Pointer    // opaque Display*
	xlibWindow C.ulong
	xlibHandle unsafe.Pointer    // dlopen handle
}

// VkSurfacePtr returns the raw VkSurfaceKHR handle as unsafe.Pointer.
// Returns nil when s is nil (maps to VK_NULL_HANDLE on the C side).
func (s *HeadlessSurface) VkSurfacePtr() unsafe.Pointer {
	if s == nil {
		return nil
	}
	return unsafe.Pointer(s.surface)
}

// Destroy destroys the VkSurfaceKHR and any associated X11 resources.
// instance must be the VkInstance that was used to create the surface.
// Safe to call on a nil receiver.
func (s *HeadlessSurface) Destroy(instance unsafe.Pointer) {
	if s == nil || instance == nil {
		return
	}
	C.cloudplay_destroy_surface((C.VkInstance)(instance), s.surface)
	s.surface = nil
	if s.xlibDpy != nil {
		C.cloudplay_close_xlib(s.xlibDpy, s.xlibWindow, s.xlibHandle)
		s.xlibDpy = nil
		s.xlibHandle = nil
	}
}

// CreateHeadlessSurface creates a VkSurfaceKHR for headless rendering.
//
// Strategy:
//  1. Try VK_EXT_headless_surface (no display dependency).
//  2. Fall back to VK_KHR_xlib_surface via the container's Xvfb at :99.
//
// instance must have been created with at least one of those extensions
// (NewInstanceOnly in context.go guarantees this).
func CreateHeadlessSurface(instance unsafe.Pointer) (*HeadlessSurface, error) {
	inst := (C.VkInstance)(instance)
	var surface C.VkSurfaceKHR

	// ── Path 1: VK_KHR_xlib_surface via Xvfb :99 ────────────────────────
	// Preferred when Xvfb is running because NVIDIA's Xlib surface fully
	// supports format/present-mode queries that cores need for swapchain
	// creation.  VK_EXT_headless_surface on some NVIDIA drivers returns
	// VK_ERROR_SURFACE_LOST_KHR (-13) from vkGetPhysicalDeviceSurfaceFormatsKHR.
	var dpy unsafe.Pointer
	var win C.ulong
	var libx11 unsafe.Pointer
	if res := C.cloudplay_create_xlib_surface(inst, &surface, &dpy, &win, &libx11); res == C.VK_SUCCESS {
		return &HeadlessSurface{
			surface:    surface,
			xlibDpy:    dpy,
			xlibWindow: win,
			xlibHandle: libx11,
		}, nil
	}

	// ── Path 2: VK_EXT_headless_surface (no display fallback) ─────────────
	if res := C.cloudplay_create_headless_surface(inst, &surface); res == C.VK_SUCCESS {
		return &HeadlessSurface{surface: surface}, nil
	}

	return nil, fmt.Errorf("vulkan: surface: both VK_KHR_xlib_surface and VK_EXT_headless_surface failed")
}

// CheckSurfacePresent reports whether the physical device can present to the
// surface on the given queue family.
func CheckSurfacePresent(physDevice unsafe.Pointer, queueFamily uint32, surface *HeadlessSurface) bool {
	if surface == nil {
		return false
	}
	rc := C.cloudplay_check_surface_support(
		(C.VkPhysicalDevice)(physDevice),
		C.uint32_t(queueFamily),
		surface.surface,
	)
	return rc > 0
}

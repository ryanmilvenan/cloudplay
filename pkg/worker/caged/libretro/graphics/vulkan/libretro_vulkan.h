/* libretro_vulkan.h — Vulkan-specific libretro interface definitions.
 *
 * Derived from RetroArch libretro-common/include/libretro_vulkan.h (MIT licence).
 * Included in the CloudPlay Vulkan graphics package so CGo can reference
 * these types without needing a full RetroArch checkout.
 *
 * Phase 3 fix: updated to the correct RETRO_HW_RENDER_INTERFACE_VULKAN_VERSION 5
 * struct layout, which adds get_device_proc_addr / get_instance_proc_addr before
 * queue.  The old version=1 layout was missing these two fields, causing Dolphin
 * to read VkQueue as a function pointer → Bus error on the first retro_run().
 */
#ifndef LIBRETRO_VULKAN_H__
#define LIBRETRO_VULKAN_H__

#include <vulkan/vulkan.h>
#include <stdint.h>
#include <stdbool.h>

#define RETRO_HW_RENDER_INTERFACE_VULKAN         0  /* retro_hw_render_interface_type enum value */
#define RETRO_HW_RENDER_INTERFACE_VULKAN_VERSION 5
#define RETRO_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_VULKAN_VERSION 2

/* ── Callback typedefs ──────────────────────────────────────────────────── */

struct retro_vulkan_image;

/* Core calls set_image() after retro_run() to hand the rendered frame to
 * the frontend.  src_queue_family may be QUEUE_FAMILY_IGNORED if there is
 * no queue ownership transfer needed. */
typedef void (*retro_vulkan_set_image_t)(
        void *handle,
        const struct retro_vulkan_image *image,
        uint32_t num_semaphores,
        const VkSemaphore *semaphores,
        uint32_t src_queue_family);

/* Returns the current swap-chain index (0 or 1 in a double-buffered setup).
 * The core uses this to pick the right sync resources. */
typedef uint32_t (*retro_vulkan_get_sync_index_t)(void *handle);

/* Returns a bitmask of valid sync indices (usually 0x3 for double-buffer). */
typedef uint32_t (*retro_vulkan_get_sync_index_mask_t)(void *handle);

/* Core submits its recorded command buffers. */
typedef void (*retro_vulkan_set_command_buffers_t)(
        void *handle,
        uint32_t num_cmd,
        const VkCommandBuffer *cmd);

/* Frontend blocks until the given sync index is no longer in flight.
 * NOTE: signature is (handle) only — no index parameter (per spec v5). */
typedef void (*retro_vulkan_wait_sync_index_t)(void *handle);

/* Acquire exclusive access to the shared graphics queue. */
typedef void (*retro_vulkan_lock_queue_t)(void *handle);

/* Release exclusive access to the shared graphics queue. */
typedef void (*retro_vulkan_unlock_queue_t)(void *handle);

/* Core provides a semaphore for the frontend to signal when it has
 * finished consuming the current frame. */
typedef void (*retro_vulkan_set_signal_semaphore_t)(void *handle, VkSemaphore semaphore);

typedef const VkApplicationInfo *(*retro_vulkan_get_application_info_t)(void);

/* ── retro_vulkan_image ──────────────────────────────────────────────────── */

/* The core hands this to the frontend after each retro_run() via set_image.
 *
 * Standard libretro spec: image_view + image_layout + create_info.
 * The underlying VkImage can be obtained from create_info.image (which is
 * populated by the core when it creates the image view).
 *
 * NOTE: we deliberately do NOT add any CloudPlay-extension fields here,
 * since the core allocates this struct and passes a pointer — if our header
 * adds fields the core doesn't know about, we'd read out-of-bounds.
 */
struct retro_vulkan_image
{
    VkImageView           image_view;
    VkImageLayout         image_layout;
    VkImageViewCreateInfo create_info;   /* includes format, extent, and image */
};

/* ── Context block (forward-declared here so typedefs can reference it) ─── */

/* Context block that the core fills in during create_device negotiation. */
struct retro_vulkan_context
{
    VkPhysicalDevice gpu;
    VkDevice         device;
    VkQueue          queue;
    uint32_t         queue_family_index;
    VkQueue          presentation_queue;       /* may equal queue */
    uint32_t         presentation_queue_family_index;
};

/* ── Negotiation interface (frontend → core) ────────────────────────────── */

typedef bool (*retro_vulkan_create_device_t)(
      struct retro_vulkan_context *context,
      VkInstance instance,
      VkPhysicalDevice gpu,
      VkSurfaceKHR surface,
      PFN_vkGetInstanceProcAddr get_instance_proc_addr,
      const char **required_device_extensions,
      unsigned num_required_device_extensions,
      const char **required_device_layers,
      unsigned num_required_device_layers,
      const VkPhysicalDeviceFeatures *required_features);

typedef void (*retro_vulkan_destroy_device_t)(void);

/* v2 CONTEXT_NEGOTIATION_INTERFACE only. */
typedef VkInstance (*retro_vulkan_create_instance_wrapper_t)(
      void *opaque, const VkInstanceCreateInfo *create_info);

typedef VkInstance (*retro_vulkan_create_instance_t)(
      PFN_vkGetInstanceProcAddr get_instance_proc_addr,
      const VkApplicationInfo *app,
      retro_vulkan_create_instance_wrapper_t create_instance_wrapper,
      void *opaque);

typedef VkDevice (*retro_vulkan_create_device_wrapper_t)(
      VkPhysicalDevice gpu, void *opaque,
      const VkDeviceCreateInfo *create_info);

typedef bool (*retro_vulkan_create_device2_t)(
      struct retro_vulkan_context *context,
      VkInstance instance,
      VkPhysicalDevice gpu,
      VkSurfaceKHR surface,
      PFN_vkGetInstanceProcAddr get_instance_proc_addr,
      retro_vulkan_create_device_wrapper_t create_device_wrapper,
      void *opaque);

/* Passed to the core inside RETRO_ENVIRONMENT_SET_HW_RENDER so it knows how
 * to negotiate a compatible Vulkan instance/device with us. */
struct retro_hw_render_context_negotiation_interface_vulkan
{
    /* Must equal RETRO_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_VULKAN */
    unsigned interface_type;
    /* Usually RETRO_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_VULKAN_VERSION */
    unsigned interface_version;

    retro_vulkan_get_application_info_t get_application_info;
    retro_vulkan_create_device_t        create_device;
    retro_vulkan_destroy_device_t       destroy_device;

    /* v2 only — NULL when interface_version < 2 */
    retro_vulkan_create_instance_t      create_instance;
    retro_vulkan_create_device2_t       create_device2;
};

/* ── Runtime interface (frontend → core, returned via GET_HW_RENDER_INTERFACE)
 *
 * IMPORTANT (Phase 3 fix):
 * The field order here MUST exactly match RetroArch's libretro_vulkan.h v5.
 * Specifically, get_device_proc_addr and get_instance_proc_addr come BEFORE
 * queue/queue_index.  Our previous v1 header was missing these two fields,
 * causing Dolphin to treat VkQueue as a function pointer → Bus error.
 */
struct retro_hw_render_interface_vulkan
{
    /* Must equal RETRO_HW_RENDER_INTERFACE_VULKAN (== 0) */
    unsigned interface_type;
    /* Must equal RETRO_HW_RENDER_INTERFACE_VULKAN_VERSION (== 5) */
    unsigned interface_version;

    /* Opaque handle passed back in all callback invocations. */
    void *handle;

    /* Vulkan instance/device the core may use directly. */
    VkInstance       instance;
    VkPhysicalDevice gpu;
    VkDevice         device;

    /* ── v5 additions: proc addr loaders ──────────────────────────────────
     * Added in interface_version 5.  Must be populated with the standard
     * Vulkan loader functions so the core can resolve additional entry points
     * without linking against libvulkan itself.
     *
     * These are the fields that were MISSING in our v1 header — their absence
     * caused the Bus error (Dolphin read VkQueue at this offset and called
     * it as a function pointer).
     */
    PFN_vkGetDeviceProcAddr   get_device_proc_addr;
    PFN_vkGetInstanceProcAddr get_instance_proc_addr;

    /* Queue the core must use to submit work. */
    VkQueue  queue;
    unsigned queue_index;

    /* Callbacks for the core to drive the frame loop. */
    retro_vulkan_set_image_t              set_image;
    retro_vulkan_get_sync_index_t         get_sync_index;
    retro_vulkan_get_sync_index_mask_t    get_sync_index_mask;
    retro_vulkan_set_command_buffers_t    set_command_buffers;
    retro_vulkan_wait_sync_index_t        wait_sync_index;
    retro_vulkan_lock_queue_t             lock_queue;
    retro_vulkan_unlock_queue_t           unlock_queue;
    retro_vulkan_set_signal_semaphore_t   set_signal_semaphore;

    /* NULL when we don't implement the negotiation interface. */
    const struct retro_hw_render_context_negotiation_interface_vulkan *negotiation_interface;
};

#endif /* LIBRETRO_VULKAN_H__ */

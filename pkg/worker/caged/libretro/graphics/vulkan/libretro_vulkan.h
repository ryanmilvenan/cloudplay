/* libretro_vulkan.h — Vulkan-specific libretro interface definitions.
 *
 * Derived from RetroArch libretro_vulkan.h (MIT licence).
 * Included in the CloudPlay Vulkan graphics package so CGo can reference
 * these types without needing a full RetroArch checkout.
 */
#ifndef LIBRETRO_VULKAN_H__
#define LIBRETRO_VULKAN_H__

#include <vulkan/vulkan.h>
#include <stdint.h>
#include <stdbool.h>

#define RETRO_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_VULKAN_VERSION 1

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

/* Frontend blocks until the given sync index is no longer in flight. */
typedef void (*retro_vulkan_wait_sync_index_t)(void *handle, unsigned index);

/* Acquire exclusive access to the shared graphics queue. */
typedef void (*retro_vulkan_lock_queue_t)(void *handle);

/* Release exclusive access to the shared graphics queue. */
typedef void (*retro_vulkan_unlock_queue_t)(void *handle);

/* Core provides a semaphore for the frontend to signal when it has
 * finished consuming the current frame. */
typedef void (*retro_vulkan_set_signal_semaphore_t)(void *handle, VkSemaphore semaphore);

/* ── retro_vulkan_image ──────────────────────────────────────────────────── */

/* The core hands this to the frontend after each retro_run() via set_image.
 *
 * RetroArch convention (from libretro-common/include/libretro_vulkan.h):
 * The core provides the create_info so the frontend can know the format and
 * extent, and image_view is the view into the underlying VkImage.
 * For readback purposes, the frontend needs the underlying VkImage; the core
 * is expected to have created the image via the device we provided, so we can
 * obtain it from the image view's backing image.
 *
 * We add an explicit `image` field here (a CloudPlay extension) so that the
 * core can hand us the VkImage directly if it wishes to, while keeping
 * compatibility with cores that only fill in image_view/image_layout.
 */
struct retro_vulkan_image
{
    VkImageView           image_view;
    VkImageLayout         image_layout;
    VkImageViewCreateInfo create_info;   /* includes format, extent etc. */

    /* CloudPlay extension: if non-NULL, the underlying VkImage.
     * Cores that follow the standard spec may leave this NULL; in that case
     * the frontend falls back to resolving the image from the view. */
    VkImage               image;
};

/* ── Negotiation interface (frontend → core) ────────────────────────────── */

/* Passed to the core inside RETRO_ENVIRONMENT_SET_HW_RENDER so it knows how
 * to negotiate a compatible Vulkan instance/device with us. */
struct retro_hw_render_context_negotiation_interface_vulkan
{
    /* Must equal RETRO_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_VULKAN */
    unsigned interface_type;
    /* Must equal RETRO_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE_VULKAN_VERSION */
    unsigned interface_version;

    /* Frontend's application info.  Core may use it to populate its own
     * VkApplicationInfo (or pass NULL to let us choose). */
    const VkApplicationInfo *(*get_application_info)(void);

    /* Called by the core to negotiate and create the shared VkDevice. The
     * frontend provides the pre-created instance; the core may add required
     * extensions and features, then calls vkCreateDevice itself. */
    bool (*create_device)(
        struct retro_vulkan_context *context,
        VkInstance instance,
        VkPhysicalDevice gpu,            /* VK_NULL_HANDLE → frontend chooses */
        VkSurfaceKHR surface,            /* VK_NULL_HANDLE for headless */
        PFN_vkGetInstanceProcAddr get_instance_proc_addr,
        const char **required_device_extensions,
        unsigned num_required_device_extensions,
        const char **required_device_layers,
        unsigned num_required_device_layers,
        const VkPhysicalDeviceFeatures *required_features);

    /* Optional: frontend tears down a device created via create_device. */
    void (*destroy_device)(void);
};

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

/* ── Runtime interface (frontend → core, returned via GET_HW_RENDER_INTERFACE) */

struct retro_hw_render_interface_vulkan
{
    /* Must equal RETRO_HW_RENDER_INTERFACE_VULKAN (== 0) */
    unsigned interface_type;
    /* Version of this interface */
    unsigned interface_version;

    /* Opaque handle passed back in all callback invocations. */
    void *handle;

    /* Vulkan handles the core can use directly. */
    VkInstance       instance;
    VkPhysicalDevice gpu;
    VkDevice         device;
    VkQueue          queue;
    unsigned         queue_index;

    /* Callbacks for the core to drive the frame loop. */
    retro_vulkan_set_image_t              set_image;
    retro_vulkan_get_sync_index_t         get_sync_index;
    retro_vulkan_get_sync_index_mask_t    get_sync_index_mask;
    retro_vulkan_set_command_buffers_t    set_command_buffers;
    retro_vulkan_wait_sync_index_t        wait_sync_index;
    retro_vulkan_lock_queue_t             lock_queue;
    retro_vulkan_unlock_queue_t           unlock_queue;
    retro_vulkan_set_signal_semaphore_t   set_signal_semaphore;

    const struct retro_hw_render_context_negotiation_interface_vulkan *negotiation_interface;
};

#endif /* LIBRETRO_VULKAN_H__ */

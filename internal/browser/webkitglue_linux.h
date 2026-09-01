// The C half of the WebKitGTK engine (spec docs/specs/embedded-browser.md §6).
//
// Only what NEEDS C lives here: GTK/WebKit signal handlers (which require real
// C function pointers), the two asynchronous WebKit calls whose completion
// callbacks must be C, and the window surgery the pane host depends on.
// Everything else is called from Go through cgo directly.
//
// Every declaration uses void* rather than a GTK type so the Go file carrying
// the //export callbacks can include this header without pulling in glib's
// static inline definitions, which cgo refuses in an exporting file.

#ifndef AO_WEBKIT_GLUE_H
#define AO_WEBKIT_GLUE_H

#include <stdint.h>

// ---- host ----------------------------------------------------------------

// ao_wk_host_attach wraps the Wails window's existing child in a GtkOverlay and
// adds the 1x1 clipping background host beneath it. Idempotent per window.
// Returns 1 on success, 0 when the window has no child to wrap.
int ao_wk_host_attach(void *gtk_window);

// ao_wk_host_park puts a view into the background host at its own slot. The
// host clips to 1x1, so a parked view is mapped (real viewport, fresh
// snapshots) while costing the window no size at all. A GtkFixed at offscreen
// coordinates would instead propagate its size and balloon the window.
void ao_wk_host_park(void *view, int slot, int width, int height);

// ao_wk_host_unpark removes a view from the background host.
void ao_wk_host_unpark(void *view);

// ao_wk_host_present positions one view over the pane's content rect, always as
// four GtkOverlay margins with ALIGN_FILL. gtk_widget_set_size_request cannot
// SHRINK a WebKitWebView — its natural size sticks at the largest-ever
// allocation — so a size-request pane would only ever grow.
// The rect is in the SPA's CSS pixels; vw/vh are the SPA viewport it was
// measured in. The overlay's own size over that viewport is the scale, which
// keeps the view aligned under webview zoom without either side knowing the
// zoom factor. vw/vh <= 0 means the rect is already in overlay units.
// clip_* is the VISIBLE intersection of that rect, same units. clip == rect is
// the unclipped presentation and is byte-for-byte the path above; a smaller
// clip moves the view into a clipping box sized to the intersection, where it
// keeps the FULL rect's size so a half-occluded page does not relayout.
void ao_wk_host_present(void *view, double x, double y, double width,
                        double height, double clip_x, double clip_y,
                        double clip_width, double clip_height, double vw,
                        double vh);

// ao_wk_host_hide returns a presented view to the background host without
// tearing anything down.
void ao_wk_host_hide(void *view);

// ao_wk_host_presented reports whether this view is the presented one, which is
// what decides whether a dialog, picker, or context menu is shown or answered.
int ao_wk_host_presented(void *view);

// ---- session -------------------------------------------------------------

// ao_wk_session_new creates one workspace's isolated WebKitNetworkSession over
// an AO-owned data directory, or an ephemeral one. Never touches the default
// session, which would write into ~/.local/share/webkitgtk/.
void *ao_wk_session_new(const char *data_dir, const char *cache_dir,
                        const char *cookie_file, const char *download_dir,
                        int ephemeral, uint64_t profile_id);
void ao_wk_session_free(void *session);

// ---- view ----------------------------------------------------------------

// ao_wk_view_new creates a hidden page view bound to a session, with the
// console capture script installed and every host delegate connected.
void *ao_wk_view_new(void *session, uint64_t page_id, const char *user_script,
                     const char *console_handler);

// ao_wk_view_adopt connects the delegates to a view the engine created itself
// (a popup) once the Manager has decided to keep it.
void ao_wk_view_adopt(void *view, uint64_t page_id, const char *user_script,
                      const char *console_handler);

void ao_wk_view_close(void *view);
// ao_wk_view_set_background sets the view's base color (opaque), which is what
// the page paints over and what shows where it has not painted yet.
void ao_wk_view_set_background(void *view, double red, double green, double blue);
void ao_wk_view_set_size(void *view, int width, int height);
// ao_wk_view_open_inspector shows the WebKit inspector for one view.
// Developer extras are enabled at view construction, so the inspector is
// always available to show; WebKitGTK docks it inside the app window.
void ao_wk_view_open_inspector(void *view);
void ao_wk_view_load_uri(void *view, const char *uri);
void ao_wk_view_history(void *view, int action);
int ao_wk_view_is_loading(void *view);
// ao_wk_view_can_go reports whether a back (forward=0) or forward (forward=1)
// history entry exists, so the driver refuses the move instead of silently
// doing nothing.
int ao_wk_view_can_go(void *view, int forward);

// ao_wk_view_eval evaluates one async-function body and reports the JSON result
// to aoWebKitEvalDone under call_id.
void ao_wk_view_eval(void *view, const char *body, uint64_t call_id);

// ao_wk_view_snapshot captures the view and reports premultiplied BGRA pixels
// to aoWebKitSnapshotDone under call_id. Works on a parked (mapped but
// clipped) view and returns fresh pixels after a DOM mutation.
void ao_wk_view_snapshot(void *view, int full_document, uint64_t call_id);

// ao_wk_policy_finish answers a navigation decision aoWebKitAllow deferred, and
// drops the reference the delegate took to hold it. Exactly one call per
// deferred decision.
void ao_wk_policy_finish(void *decision, int allow);

// ---- memory --------------------------------------------------------------

// ao_wk_free releases memory the C half allocated for the Go half. GLib's
// allocator is not required to be the C library's, so a g_strdup'd string must
// never be handed to free().
void ao_wk_free(void *pointer);

// ---- downloads -----------------------------------------------------------

void ao_wk_download_cancel(void *download);
void ao_wk_download_unref(void *download);

#endif

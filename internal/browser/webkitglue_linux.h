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

// ao_wk_on_main_thread reports whether the caller is the GTK thread: the one
// ao_wk_host_attach ran on, the one every call here runs on, and the one Wails
// blocks while services shut down. The Manager asks it to choose an inline
// teardown over a fanned-out one. 0 until the host has attached once.
int ao_wk_on_main_thread(void);

// ao_wk_host_park puts a view's host into the background park at its own
// slot. The park clips to 1x1, so a parked view is mapped at its full
// viewport (real layout, fresh snapshots) while costing the window no size at
// all. A GtkFixed at offscreen coordinates would instead propagate its size
// and balloon the window.
void ao_wk_host_park(void *view, int slot);

// ao_wk_host_unpark removes a view's host from whichever container holds it.
void ao_wk_host_unpark(void *view);

// ao_wk_host_present shows one view inside the pane: its host becomes an
// overlay child filling the pane's VISIBLE clip rect (four margins with
// ALIGN_FILL), and the view is drawn inside it at the FITTED rect's offset,
// scaled. The view's own allocation stays its viewport (ao_wk_view_set_size);
// the pane never resizes a page, it shows it scaled to fit.
// x/y/width/height is the fitted rect and clip_* its visible intersection,
// both in the SPA's CSS pixels; vw/vh are the SPA viewport they were measured
// in, and the overlay's own size over that viewport is the CSS-to-overlay
// proportion. vw/vh <= 0 means the rects are already in overlay units. scale
// is the fitted rect's size over the viewport, in CSS pixels.
void ao_wk_host_present(void *view, double x, double y, double width,
                        double height, double clip_x, double clip_y,
                        double clip_width, double clip_height, double vw,
                        double vh, double scale);

// ao_wk_host_hide ends a presentation and parks the view at slot. A view
// that held keyboard focus hands it to the SPA's webview: it answers 1 when
// it did, -1 when the view held focus and the handoff failed, 0 otherwise.
int ao_wk_host_hide(void *view, int slot);

// ao_wk_host_presented reports whether this view is presented, which is what
// decides whether a dialog, picker, or context menu is shown or answered.
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
// (a popup) once the Manager has decided to keep it. The popup's navigation
// policy is connected when WebKit creates it, and answers with its
// workspace's policy until this stamps the page id.
void ao_wk_view_adopt(void *view, uint64_t page_id, const char *user_script,
                      const char *console_handler);

// ao_wk_view_close closes the view, handing keyboard focus it held to the
// SPA's webview, and answers as ao_wk_host_hide does.
int ao_wk_view_close(void *view);
// ao_wk_view_set_background sets the view's base color (opaque), which is what
// the page paints over and what shows where it has not painted yet.
void ao_wk_view_set_background(void *view, double red, double green, double blue);
// ao_wk_view_set_size sets the page's viewport: the exact size its host
// allocates it, parked or presented.
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
// to aoWebKitEvalDone under call_id. world names an isolated script world, or
// is NULL for the page's own.
void ao_wk_view_eval(void *view, const char *body, const char *world, uint64_t call_id);

// ao_wk_view_snapshot captures the view and reports premultiplied BGRA pixels
// to aoWebKitSnapshotDone under call_id. Works on a parked (mapped but
// clipped) view and returns fresh pixels after a DOM mutation.
void ao_wk_view_snapshot(void *view, int full_document, uint64_t call_id);

// ao_wk_policy_finish answers a navigation decision aoWebKitAllow deferred, and
// drops the reference the delegate took to hold it. Exactly one call per
// deferred decision.
void ao_wk_policy_finish(void *decision, int allow);

// ---- find ----------------------------------------------------------------

#define AO_FIND_SEARCH 0
#define AO_FIND_NEXT 1
#define AO_FIND_PREVIOUS 2

// ao_wk_view_find runs one find on the view's WebKitFindController: a
// case-insensitive, wrapping search for text from the start of the document
// (AO_FIND_SEARCH), or a step from the current match. The outcome reaches
// aoWebKitFindDone under call_id: found 1 with the match count (G_MAXUINT past
// max_match_count), found 0 with none, or found -1 when the view has no find.
void ao_wk_view_find(void *view, int op, const char *text, unsigned int max_match_count,
                     uint64_t call_id);
// ao_wk_view_find_finish ends the view's find and unhighlights its matches.
void ao_wk_view_find_finish(void *view);

// ao_wk_view_offer_escape delivers one Escape press (with Control when ctrl),
// then its release when release, to the view's own key controller as GTK
// would, and answers 1 when AO stopped the press, 0 when the page keeps it,
// -1 for a view without one. It reaches AO's gate, not the page: GTK 4
// cannot make a real input event. The native smoke's door into the gate.
int ao_wk_view_offer_escape(void *view, int ctrl, int release);

// ao_wk_view_offer_ctrl_key delivers one Control press of the key whose
// character is unicode to the view's key controller, as
// ao_wk_view_offer_escape does, and answers the same way.
int ao_wk_view_offer_ctrl_key(void *view, unsigned int unicode);

// ao_wk_focus_app gives keyboard focus to the Wails SPA's webview, the
// handoff of a claimed chord, and answers 1 when GTK made it the window's
// focus widget, 0 without one.
int ao_wk_focus_app(void);

// For the native smoke: ao_wk_view_grab_focus gives the view keyboard
// focus; ao_wk_focus_holder answers which widget the view's window has as
// its focus widget: 1 the view (or inside it), 2 the SPA's webview, 3 any
// other, 0 none; ao_wk_window_active whether that window is active.
void ao_wk_view_grab_focus(void *view);
int ao_wk_focus_holder(void *view);
int ao_wk_window_active(void *view);

// ao_wk_view_has_input_method answers whether the view has an input method
// context, the one the key gate reads a composition from.
int ao_wk_view_has_input_method(void *view);

// ---- memory --------------------------------------------------------------

// ao_wk_free releases memory the C half allocated for the Go half. GLib's
// allocator is not required to be the C library's, so a g_strdup'd string must
// never be handed to free().
void ao_wk_free(void *pointer);

// ---- downloads -----------------------------------------------------------

void ao_wk_download_cancel(void *download);
void ao_wk_download_unref(void *download);

#endif

// The Objective-C half of the WKWebView engine (spec docs/specs/embedded-browser.md §6).
//
// Only what NEEDS Objective-C lives here: the WKWebView delegates (which are
// protocol methods on a real class), the two asynchronous WebKit calls whose
// completion handlers must be blocks, the AppKit dialogs WKWebView has no
// built-in of, and the view surgery the pane host depends on. Everything else
// is called from Go through cgo directly.
//
// Every declaration uses void* rather than an Objective-C type so the Go file
// carrying the //export callbacks can include this header without pulling in
// Cocoa's headers, which cgo refuses in an exporting file.

#ifndef AO_WKWEBVIEW_GLUE_H
#define AO_WKWEBVIEW_GLUE_H

#include <stdint.h>

// ---- availability --------------------------------------------------------

// ao_wkv_supported reports whether this macOS can host the engine at all.
// -callAsyncJavaScript:arguments:inFrame:inContentWorld:completionHandler: is
// macOS 11.0, and it is the ONE call every page operation goes through: there
// is no engine without it. An older macOS answers 0 and keeps managed Chrome,
// which is a capability answer exactly like "is there a window".
int ao_wkv_supported(void);

// ao_wkv_on_main_thread reports whether the caller is AppKit's main thread: the
// thread every call here runs on, and the one Wails blocks while services shut
// down. The Manager asks it to choose an inline teardown over a fanned-out one.
int ao_wkv_on_main_thread(void);

// ---- host ----------------------------------------------------------------

// ao_wkv_host_attach adds AO's 1x1 clipping park view BENEATH the Wails
// window's existing subviews (the SPA WKWebView), so its footprint is painted
// over and it can never take a click. Idempotent per process.
// Returns 1 on success, 0 when the window has no content view.
int ao_wkv_host_attach(void *ns_window);

// ao_wkv_host_park puts a view into the park view at its own slot. The park
// view is 1x1 with a masked layer, so a parked view is IN THE WINDOW (real
// viewport, snapshot-able) while costing the window no space at all.
void ao_wkv_host_park(void *view, int slot, int width, int height);

// ao_wkv_host_unpark removes a view from whichever AO host holds it.
void ao_wkv_host_unpark(void *view);

// ao_wkv_host_present positions one view over the pane's content rect, above
// the SPA. The rect arrives in top-left CSS coordinates and is flipped here
// against the content view's own geometry.
// The rect is in the SPA's CSS pixels with (vw, vh) its viewport size; the
// host scales by its own bounds over that viewport, so webview zoom and DPI
// need no scale factor on either side. vw <= 0 presents unscaled.
//
// (clip_x, clip_y, clip_width, clip_height) is the VISIBLE intersection of
// that rect, in the same units: the view keeps the FULL rect's size — a page
// must not relayout because it scrolled half behind the sidebar — and the
// page's OWN clipping container, created here on first present, crops what is
// presented. Per page, because two threads with a visible pane each present a
// page at the same moment. An absent clip pair means unclipped, which presents
// exactly as it did before clipping existed.
//
// bg is the pane's resolved background as packed 0xRRGGBB, or negative for
// none. It is painted where the page has not presented yet, so a strip
// exposed by a resize matches the pane instead of the engine default.
void ao_wkv_host_present(void *view, double x, double y, double width, double height,
                         double clip_x, double clip_y, double clip_width, double clip_height,
                         double vw, double vh, int bg);

// ao_wkv_host_hide stops presenting THIS view without tearing anything down:
// its clip container is hidden and no other page's presentation is touched.
// The caller parks the view immediately afterwards, as on Linux. The container
// itself lives until the page closes, so re-showing costs no view surgery.
void ao_wkv_host_hide(void *view);

// ao_wkv_host_presented reports whether this view is currently presented, which
// is what decides whether a dialog or file picker is shown or answered. It is
// read off the view tree — the page inside its own showing container — so it
// needs no bookkeeping of its own and stays right for every page at once.
int ao_wkv_host_presented(void *view);

// ---- website data store --------------------------------------------------

// ao_wkv_store_new creates one workspace's isolated WKWebsiteDataStore.
// A persistent store needs +dataStoreForIdentifier: (macOS 14); an older macOS,
// or the site-data setting being off, gets +nonPersistentDataStore. Never the
// default store, which is the SPA webview's own.
void *ao_wkv_store_new(const char *identifier_uuid, int ephemeral);
void ao_wkv_store_free(void *store);

// ao_wkv_clear_data removes EVERY WKWebsiteDataStore this app created and
// reports completion to aoWKVClearDone under call_id: a NULL error on success,
// otherwise the per-store failure descriptions joined by newlines, which the Go
// half folds into one message.
//
// "All of them" IS exactly this engine's site data. The only enumerable stores
// are the ones +dataStoreForIdentifier: made, every one of them is a workspace
// store AO asked for inside this app's own container, and the SPA webview's
// default store carries no identifier and is therefore never returned.
//
// Below macOS 14 there is no +dataStoreForIdentifier: at all: every workspace
// ran on +nonPersistentDataStore, nothing persistent was ever written, and
// there is genuinely nothing to remove. That is reported as SUCCESS with zero
// identifiers, never as an error.
void ao_wkv_clear_data(uint64_t call_id);

// ---- view ----------------------------------------------------------------

// ao_wkv_view_new creates a hidden page view bound to a store, with the console
// capture script installed and every delegate connected.
void *ao_wkv_view_new(void *store, uint64_t page_id, uint64_t profile_id,
                      const char *user_script, const char *console_handler,
                      const char *download_dir);

// ao_wkv_view_adopt connects the delegates to a view the engine created itself
// (a popup) once the Manager has decided to keep it. The popup's configuration
// may share the opener's user content controller, so the user script is added
// only when that controller carries none.
void ao_wkv_view_adopt(void *view, uint64_t page_id, uint64_t profile_id,
                       const char *user_script, const char *console_handler,
                       const char *download_dir);

void ao_wkv_view_close(void *view);
void ao_wkv_view_set_size(void *view, int width, int height);
// ao_wkv_view_get_size reads the view's current size, which a full-document
// screenshot restores after capturing at the document's size.
void ao_wkv_view_get_size(void *view, int *width, int *height);
void ao_wkv_view_load_uri(void *view, const char *uri);
// ao_wkv_view_load_file is the file:// path: WKWebView refuses a file URL
// through -loadRequest:, and -loadFileURL:allowingReadAccessToURL: is the
// documented way to grant a page access to its own directory.
void ao_wkv_view_load_file(void *view, const char *path, const char *read_access_dir);
void ao_wkv_view_history(void *view, int action);
int ao_wkv_view_is_loading(void *view);
// ao_wkv_view_can_go reports whether a back (forward=0) or forward (forward=1)
// history entry exists, so the driver refuses the move instead of silently
// doing nothing.
int ao_wkv_view_can_go(void *view, int forward);

// ao_wkv_view_eval evaluates one async-function body and reports the JSON
// result to aoWKVEvalDone under call_id.
void ao_wkv_view_eval(void *view, const char *body, uint64_t call_id);

// ao_wkv_view_snapshot captures the view's CURRENT size and reports
// premultiplied BGRA pixels to aoWKVSnapshotDone under call_id, one image pixel
// per CSS pixel. The caller resizes the view first when it wants the whole
// document: WKSnapshotConfiguration cannot capture past the view's bounds.
void ao_wkv_view_snapshot(void *view, uint64_t call_id);

// ao_wkv_policy_finish answers a navigation decision aoWKVAllow deferred, and
// releases the copied decision block. Exactly one call per deferred decision.
void ao_wkv_policy_finish(void *decision, int allow);

// ao_wkv_view_press_key makes the view first responder and sends one key-down
// through its window's own event path, so a chord is gated exactly as a real
// keystroke would be. Harness-only; key is the KeyboardEvent.key spelling.
void ao_wkv_view_press_key(void *view, const char *key, int ctrl, int meta, int alt, int shift);

// ---- memory --------------------------------------------------------------

// ao_wkv_free releases memory the Objective-C half allocated for the Go half.
// Everything crossing this boundary is malloc'd, so this is plain free().
void ao_wkv_free(void *pointer);

// ---- downloads -----------------------------------------------------------

void ao_wkv_download_cancel(void *download);
void ao_wkv_download_release(void *download);
// ao_wkv_download_received reads the download's NSProgress. WKDownload reports
// no per-chunk callback, so the Go side samples this while a download is live —
// which is what lets the Manager enforce its per-download byte cap mid-flight.
double ao_wkv_download_received(void *download);

#endif

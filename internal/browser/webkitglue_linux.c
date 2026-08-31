//go:build linux && cgo && !gtk3 && !android && !server && !nogui

#include "webkitglue_linux.h"

#include <gtk/gtk.h>
#include <webkit/webkit.h>
#include <string.h>
#include <stdlib.h>

// Callbacks implemented in Go (webkitexport_linux.go).
extern void aoWebKitEvalDone(uint64_t call_id, char *json, char *err);
extern void aoWebKitSnapshotDone(uint64_t call_id, void *pixels, int width,
                                 int height, int stride, char *err);
extern void aoWebKitAllow(uint64_t page_id, void *decision, char *uri);
extern void aoWebKitConsole(uint64_t page_id, char *payload);
extern void aoWebKitPageInfo(uint64_t page_id, char *uri, char *title);
extern void aoWebKitPageClosed(uint64_t page_id);
extern void aoWebKitPopup(uint64_t opener_page_id, uint64_t profile_id,
                          void *view, char *uri);
extern int aoWebKitPagePresented(uint64_t page_id);
extern void aoWebKitDownloadStarted(uint64_t profile_id, uint64_t page_id,
                                    uint64_t download_id, void *download,
                                    char *uri, char *suggested);
extern void aoWebKitDownloadProgress(uint64_t download_id, double received,
                                     int state, char *path);

// The download state vocabulary the Go seam uses (driver.go).
#define AO_DL_IN_PROGRESS 0
#define AO_DL_COMPLETED 1
#define AO_DL_CANCELED 2

#define AO_PAGE_KEY "ao-page-id"
#define AO_PROFILE_KEY "ao-profile-id"
#define AO_SESSION_DL_DIR "ao-download-dir"
#define AO_SESSION_KEY "ao-session"
#define AO_DOWNLOAD_KEY "ao-download-id"

static uint64_t ao_view_page_id(gpointer view) {
  return (uint64_t)GPOINTER_TO_SIZE(g_object_get_data(G_OBJECT(view), AO_PAGE_KEY));
}

// ---------------------------------------------------------------------------
// Host: the widget tree AO owns inside the Wails window.
//
//   GtkWindow
//   └── GtkOverlay
//       ├── main child : the Wails SPA WebKitWebView (unchanged, still live)
//       ├── overlay 1  : 1x1 clipping GtkScrolledWindow -> GtkFixed  (parked)
//       └── overlay 2  : the presented page  (ALIGN_FILL + four margins)
//
// Order matters: the background host is added BEFORE anything else so the SPA
// paints over its 1px footprint, and the presented page is added last so it
// sits on top.
// ---------------------------------------------------------------------------

static GtkWidget *ao_overlay = NULL;
static GtkWidget *ao_park = NULL; // the GtkFixed inside the clipping scroller
static GtkWidget *ao_presented = NULL;

int ao_wk_host_attach(void *gtk_window) {
  if (ao_overlay != NULL) {
    return 1;
  }
  GtkWindow *win = GTK_WINDOW(gtk_window);
  GtkWidget *existing = gtk_window_get_child(win);
  if (existing == NULL) {
    return 0;
  }
  // set_child(NULL) drops the window's last reference to the SPA view.
  g_object_ref(existing);
  gtk_window_set_child(win, NULL);
  GtkWidget *overlay = gtk_overlay_new();

  GtkWidget *scroller = gtk_scrolled_window_new();
  gtk_scrolled_window_set_policy(GTK_SCROLLED_WINDOW(scroller), GTK_POLICY_EXTERNAL,
                                 GTK_POLICY_EXTERNAL);
  gtk_scrolled_window_set_min_content_width(GTK_SCROLLED_WINDOW(scroller), 1);
  gtk_scrolled_window_set_min_content_height(GTK_SCROLLED_WINDOW(scroller), 1);
  gtk_scrolled_window_set_propagate_natural_width(GTK_SCROLLED_WINDOW(scroller), FALSE);
  gtk_scrolled_window_set_propagate_natural_height(GTK_SCROLLED_WINDOW(scroller), FALSE);
  gtk_widget_set_halign(scroller, GTK_ALIGN_START);
  gtk_widget_set_valign(scroller, GTK_ALIGN_START);
  gtk_widget_set_size_request(scroller, 1, 1);
  GtkWidget *fixed = gtk_fixed_new();
  gtk_scrolled_window_set_child(GTK_SCROLLED_WINDOW(scroller), fixed);

  gtk_overlay_add_overlay(GTK_OVERLAY(overlay), scroller);
  gtk_overlay_set_child(GTK_OVERLAY(overlay), existing);
  g_object_unref(existing);
  gtk_window_set_child(win, overlay);

  ao_overlay = overlay;
  // gtk_scrolled_window_get_child() returns the auto-inserted GtkViewport, not
  // this GtkFixed, so keep our own pointer.
  ao_park = fixed;
  return 1;
}

void ao_wk_host_park(void *view, int slot, int width, int height) {
  if (ao_park == NULL) {
    return;
  }
  GtkWidget *w = GTK_WIDGET(view);
  gtk_widget_set_size_request(w, width, height);
  if (gtk_widget_get_parent(w) == ao_park) {
    return;
  }
  ao_wk_host_unpark(view);
  gtk_fixed_put(GTK_FIXED(ao_park), w, 0, slot * (height + 10));
}

void ao_wk_host_unpark(void *view) {
  GtkWidget *w = GTK_WIDGET(view);
  GtkWidget *parent = gtk_widget_get_parent(w);
  if (parent == ao_park && ao_park != NULL) {
    gtk_fixed_remove(GTK_FIXED(ao_park), w);
  } else if (parent == ao_overlay && ao_overlay != NULL) {
    gtk_overlay_remove_overlay(GTK_OVERLAY(ao_overlay), w);
  }
}

// ao_wk_host_present positions one view over the pane's content rect. The rect
// MUST be expressed as four margins with ALIGN_FILL: gtk_widget_set_size_request
// cannot SHRINK a WebKitWebView, whose natural size sticks at its largest-ever
// allocation, so a size-request pane only ever grows.
void ao_wk_host_present(void *view, double x, double y, double width,
                        double height, double vw, double vh) {
  if (ao_overlay == NULL) {
    return;
  }
  GtkWidget *w = GTK_WIDGET(view);
  if (ao_presented != NULL && ao_presented != w) {
    ao_wk_host_hide(ao_presented);
  }
  int overlay_w = gtk_widget_get_width(ao_overlay);
  int overlay_h = gtk_widget_get_height(ao_overlay);
  // CSS pixels -> overlay logical pixels by proportion (see header).
  double sx = (vw > 0.0 && overlay_w > 0) ? overlay_w / vw : 1.0;
  double sy = (vh > 0.0 && overlay_h > 0) ? overlay_h / vh : 1.0;
  int ix = (int)(x * sx + 0.5);
  int iy = (int)(y * sy + 0.5);
  int iw = (int)(width * sx + 0.5);
  int ih = (int)(height * sy + 0.5);
  int right = overlay_w - (ix + iw);
  int bottom = overlay_h - (iy + ih);
  if (right < 0) right = 0;
  if (bottom < 0) bottom = 0;
  if (gtk_widget_get_parent(w) != ao_overlay) {
    ao_wk_host_unpark(view);
    gtk_overlay_add_overlay(GTK_OVERLAY(ao_overlay), w);
  }
  gtk_widget_set_size_request(w, -1, -1);
  gtk_widget_set_halign(w, GTK_ALIGN_FILL);
  gtk_widget_set_valign(w, GTK_ALIGN_FILL);
  gtk_widget_set_margin_start(w, ix < 0 ? 0 : ix);
  gtk_widget_set_margin_top(w, iy < 0 ? 0 : iy);
  gtk_widget_set_margin_end(w, right);
  gtk_widget_set_margin_bottom(w, bottom);
  gtk_widget_set_visible(w, TRUE);
  ao_presented = w;
}

// ao_wk_host_hide returns a presented view to the background host. Nothing is
// torn down: page state lives on.
void ao_wk_host_hide(void *view) {
  GtkWidget *w = GTK_WIDGET(view);
  if (ao_presented == w) {
    ao_presented = NULL;
  }
  gtk_widget_set_margin_start(w, 0);
  gtk_widget_set_margin_top(w, 0);
  gtk_widget_set_margin_end(w, 0);
  gtk_widget_set_margin_bottom(w, 0);
  gtk_widget_set_halign(w, GTK_ALIGN_START);
  gtk_widget_set_valign(w, GTK_ALIGN_START);
}

int ao_wk_host_presented(void *view) { return ao_presented == GTK_WIDGET(view) ? 1 : 0; }

// ---------------------------------------------------------------------------
// Downloads
// ---------------------------------------------------------------------------

static uint64_t ao_download_seq = 0;

// ao_download_destination forces the file into the profile's artifact directory
// and is where the download is REPORTED: the suggested filename the Manager
// sanitizes only exists once the response headers have been seen, and this
// runs before any byte is written.
static gboolean ao_download_destination(WebKitDownload *download, gchar *suggested,
                                        gpointer data) {
  (void)data;
  // The session stamped its artifact directory onto the download when it
  // started; WebKitGTK 6.0 offers no download -> session accessor.
  const char *dir = g_object_get_data(G_OBJECT(download), AO_SESSION_DL_DIR);
  if (dir == NULL) {
    webkit_download_cancel(download);
    return TRUE;
  }
  uint64_t id = (uint64_t)GPOINTER_TO_SIZE(g_object_get_data(G_OBJECT(download), AO_DOWNLOAD_KEY));
  // The handle IS the on-disk name. The Manager renames the finished file to a
  // sanitized unique name inside the same directory; giving it a handle-named
  // file also lets it clean up a download it had to cancel mid-flight.
  char *name = g_strdup_printf("dl-%llu", (unsigned long long)id);
  char *path = g_build_filename(dir, name, NULL);
  webkit_download_set_destination(download, path);
  g_free(path);
  g_free(name);
  WebKitNetworkSession *session = g_object_get_data(G_OBJECT(download), AO_SESSION_KEY);
  uint64_t profile_id =
      session ? (uint64_t)GPOINTER_TO_SIZE(g_object_get_data(G_OBJECT(session), AO_PROFILE_KEY)) : 0;
  WebKitWebView *view = webkit_download_get_web_view(download);
  WebKitURIRequest *request = webkit_download_get_request(download);
  const char *uri = request ? webkit_uri_request_get_uri(request) : NULL;
  aoWebKitDownloadStarted(profile_id, view ? ao_view_page_id(view) : 0, id, download,
                          uri ? g_strdup(uri) : NULL,
                          suggested ? g_strdup(suggested) : NULL);
  return TRUE;
}

static void ao_download_received(WebKitDownload *download, guint64 length, gpointer data) {
  (void)length;
  (void)data;
  uint64_t id = (uint64_t)GPOINTER_TO_SIZE(g_object_get_data(G_OBJECT(download), AO_DOWNLOAD_KEY));
  aoWebKitDownloadProgress(id, (double)webkit_download_get_received_data_length(download),
                           AO_DL_IN_PROGRESS, NULL);
}

static void ao_download_finished(WebKitDownload *download, gpointer data) {
  (void)data;
  uint64_t id = (uint64_t)GPOINTER_TO_SIZE(g_object_get_data(G_OBJECT(download), AO_DOWNLOAD_KEY));
  const char *destination = webkit_download_get_destination(download);
  aoWebKitDownloadProgress(id, (double)webkit_download_get_received_data_length(download),
                           AO_DL_COMPLETED, destination ? g_strdup(destination) : NULL);
}

static void ao_download_failed(WebKitDownload *download, GError *error, gpointer data) {
  (void)error;
  (void)data;
  uint64_t id = (uint64_t)GPOINTER_TO_SIZE(g_object_get_data(G_OBJECT(download), AO_DOWNLOAD_KEY));
  aoWebKitDownloadProgress(id, (double)webkit_download_get_received_data_length(download),
                           AO_DL_CANCELED, NULL);
}

static void ao_session_download_started(WebKitNetworkSession *session,
                                        WebKitDownload *download, gpointer data) {
  (void)data;
  uint64_t id = ++ao_download_seq;
  g_object_set_data(G_OBJECT(download), AO_DOWNLOAD_KEY, GSIZE_TO_POINTER((gsize)id));
  // WebKitGTK 6.0 has no download -> session accessor, so the two facts the
  // destination callback needs travel on the download itself.
  g_object_set_data(G_OBJECT(download), AO_SESSION_KEY, session);
  const char *dir = g_object_get_data(G_OBJECT(session), AO_SESSION_DL_DIR);
  g_object_set_data_full(G_OBJECT(download), AO_SESSION_DL_DIR,
                         dir ? g_strdup(dir) : NULL, g_free);
  // Held until the Go side releases it, so a cancel arriving after the page
  // closed cannot reach a freed object.
  g_object_ref(download);
  g_signal_connect(download, "decide-destination", G_CALLBACK(ao_download_destination), NULL);
  g_signal_connect(download, "received-data", G_CALLBACK(ao_download_received), NULL);
  g_signal_connect(download, "finished", G_CALLBACK(ao_download_finished), NULL);
  g_signal_connect(download, "failed", G_CALLBACK(ao_download_failed), NULL);
}

void ao_wk_download_cancel(void *download) {
  if (download != NULL) {
    webkit_download_cancel(WEBKIT_DOWNLOAD(download));
  }
}

void ao_wk_download_unref(void *download) {
  if (download != NULL) {
    g_object_unref(G_OBJECT(download));
  }
}

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

void *ao_wk_session_new(const char *data_dir, const char *cache_dir,
                        const char *cookie_file, const char *download_dir,
                        int ephemeral, uint64_t profile_id) {
  WebKitNetworkSession *session =
      ephemeral ? webkit_network_session_new_ephemeral()
                : webkit_network_session_new(data_dir, cache_dir);
  if (session == NULL) {
    return NULL;
  }
  g_object_set_data(G_OBJECT(session), AO_PROFILE_KEY, GSIZE_TO_POINTER((gsize)profile_id));
  g_object_set_data_full(G_OBJECT(session), AO_SESSION_DL_DIR, g_strdup(download_dir), g_free);
  if (!ephemeral && cookie_file != NULL) {
    webkit_cookie_manager_set_persistent_storage(
        webkit_network_session_get_cookie_manager(session), cookie_file,
        WEBKIT_COOKIE_PERSISTENT_STORAGE_SQLITE);
  }
  g_signal_connect(session, "download-started", G_CALLBACK(ao_session_download_started), NULL);
  return session;
}

void ao_wk_session_free(void *session) {
  if (session != NULL) {
    g_object_unref(G_OBJECT(session));
  }
}

// ---------------------------------------------------------------------------
// View delegates
// ---------------------------------------------------------------------------

static gboolean ao_decide_policy(WebKitWebView *view, WebKitPolicyDecision *decision,
                                 WebKitPolicyDecisionType type, gpointer data) {
  (void)data;
  if (type != WEBKIT_POLICY_DECISION_TYPE_NAVIGATION_ACTION &&
      type != WEBKIT_POLICY_DECISION_TYPE_NEW_WINDOW_ACTION) {
    return FALSE;
  }
  WebKitNavigationPolicyDecision *nav = WEBKIT_NAVIGATION_POLICY_DECISION(decision);
  WebKitNavigationAction *action = webkit_navigation_policy_decision_get_navigation_action(nav);
  WebKitURIRequest *request = action ? webkit_navigation_action_get_request(action) : NULL;
  const char *uri = request ? webkit_uri_request_get_uri(request) : NULL;
  if (uri == NULL) {
    webkit_policy_decision_ignore(decision);
    return TRUE;
  }
  // The decision is DEFERRED, not answered here: navigation authority is the
  // Manager's, and asking it takes Manager locks. Blocking the GTK thread on a
  // Go lock is how the whole UI freezes behind one browser operation, so the
  // decision is held with a reference and finished from a goroutine.
  g_object_ref(decision);
  aoWebKitAllow(ao_view_page_id(view), decision, g_strdup(uri));
  return TRUE;
}

void ao_wk_policy_finish(void *decision, int allow) {
  if (decision == NULL) {
    return;
  }
  if (allow) {
    webkit_policy_decision_use(WEBKIT_POLICY_DECISION(decision));
  } else {
    webkit_policy_decision_ignore(WEBKIT_POLICY_DECISION(decision));
  }
  g_object_unref(G_OBJECT(decision));
}

// ao_wk_free releases memory the C half allocated for the Go half. GLib's
// allocator is not required to be the C library's, so a g_strdup'd string must
// never be handed to free().
void ao_wk_free(void *pointer) { g_free(pointer); }

static void ao_notify_info(GObject *object, GParamSpec *spec, gpointer data) {
  (void)spec;
  (void)data;
  WebKitWebView *view = WEBKIT_WEB_VIEW(object);
  const char *uri = webkit_web_view_get_uri(view);
  const char *title = webkit_web_view_get_title(view);
  aoWebKitPageInfo(ao_view_page_id(view), uri ? g_strdup(uri) : NULL,
                   title ? g_strdup(title) : NULL);
}

static void ao_load_changed(WebKitWebView *view, WebKitLoadEvent event, gpointer data) {
  (void)data;
  if (event == WEBKIT_LOAD_COMMITTED || event == WEBKIT_LOAD_FINISHED) {
    ao_notify_info(G_OBJECT(view), NULL, NULL);
  }
}

static void ao_view_closed(WebKitWebView *view, gpointer data) {
  (void)data;
  aoWebKitPageClosed(ao_view_page_id(view));
}

// ao_script_dialog answers JavaScript dialogs the way the CDP driver does:
// dismissed, except beforeunload which is accepted so a requested navigation
// can continue. A PRESENTED page gets the engine's own dialog instead, because
// a user looking at the page is the one being asked.
static gboolean ao_script_dialog(WebKitWebView *view, WebKitScriptDialog *dialog,
                                 gpointer data) {
  (void)data;
  if (aoWebKitPagePresented(ao_view_page_id(view))) {
    return FALSE;
  }
  switch (webkit_script_dialog_get_dialog_type(dialog)) {
    case WEBKIT_SCRIPT_DIALOG_BEFORE_UNLOAD_CONFIRM:
      webkit_script_dialog_confirm_set_confirmed(dialog, TRUE);
      break;
    case WEBKIT_SCRIPT_DIALOG_CONFIRM:
      webkit_script_dialog_confirm_set_confirmed(dialog, FALSE);
      break;
    case WEBKIT_SCRIPT_DIALOG_PROMPT:
      webkit_script_dialog_prompt_set_text(dialog, "");
      break;
    default:
      break;
  }
  return TRUE;
}

// ao_file_chooser refuses a picker on a hidden agent page — nobody is there to
// answer it — and lets the engine present the real one on a presented page.
static gboolean ao_file_chooser(WebKitWebView *view, WebKitFileChooserRequest *request,
                               gpointer data) {
  (void)data;
  if (aoWebKitPagePresented(ao_view_page_id(view))) {
    return FALSE;
  }
  webkit_file_chooser_request_cancel(request);
  return TRUE;
}

// ao_context_menu suppresses the menu on a hidden agent page and shows the real
// site menu on a presented one. Appending AO's own items to that menu is the
// pane wave's work (spec §7), and lands on this same callback.
static gboolean ao_context_menu(WebKitWebView *view, WebKitContextMenu *menu,
                                WebKitHitTestResult *hit, gpointer data) {
  (void)menu;
  (void)hit;
  (void)data;
  return aoWebKitPagePresented(ao_view_page_id(view)) ? FALSE : TRUE;
}

static GtkWidget *ao_create_view(WebKitWebView *view, WebKitNavigationAction *action,
                                 gpointer data) {
  (void)data;
  // "related-view" is construct-only and inherits the opener's network session
  // and web process, which is what makes the popup part of the same workspace
  // profile. Never pass "network-session" alongside it.
  WebKitWebView *popup =
      WEBKIT_WEB_VIEW(g_object_new(WEBKIT_TYPE_WEB_VIEW, "related-view", view, NULL));
  // Hold the popup until the Manager adopts or discards it.
  g_object_ref_sink(popup);
  WebKitURIRequest *request = action ? webkit_navigation_action_get_request(action) : NULL;
  const char *uri = request ? webkit_uri_request_get_uri(request) : NULL;
  WebKitNetworkSession *session = webkit_web_view_get_network_session(view);
  uint64_t profile_id =
      session ? (uint64_t)GPOINTER_TO_SIZE(g_object_get_data(G_OBJECT(session), AO_PROFILE_KEY)) : 0;
  aoWebKitPopup(ao_view_page_id(view), profile_id, popup, uri ? g_strdup(uri) : NULL);
  return GTK_WIDGET(popup);
}

static void ao_script_message(WebKitUserContentManager *manager, JSCValue *value,
                              gpointer data) {
  (void)manager;
  uint64_t page_id = (uint64_t)GPOINTER_TO_SIZE(data);
  if (!jsc_value_is_string(value)) {
    return;
  }
  aoWebKitConsole(page_id, jsc_value_to_string(value));
}

static void ao_connect_view(WebKitWebView *view, uint64_t page_id, const char *user_script,
                            const char *console_handler) {
  g_object_set_data(G_OBJECT(view), AO_PAGE_KEY, GSIZE_TO_POINTER((gsize)page_id));
  WebKitSettings *settings = webkit_web_view_get_settings(view);
  webkit_settings_set_enable_developer_extras(settings, TRUE);
  webkit_settings_set_javascript_can_open_windows_automatically(settings, TRUE);

  WebKitUserContentManager *ucm = webkit_web_view_get_user_content_manager(view);
  if (ucm != NULL && user_script != NULL && console_handler != NULL) {
    webkit_user_content_manager_register_script_message_handler(ucm, console_handler, NULL);
    char *signal = g_strdup_printf("script-message-received::%s", console_handler);
    g_signal_connect(ucm, signal, G_CALLBACK(ao_script_message),
                     GSIZE_TO_POINTER((gsize)page_id));
    g_free(signal);
    WebKitUserScript *script = webkit_user_script_new(
        user_script, WEBKIT_USER_CONTENT_INJECT_ALL_FRAMES,
        WEBKIT_USER_SCRIPT_INJECT_AT_DOCUMENT_START, NULL, NULL);
    webkit_user_content_manager_add_script(ucm, script);
    webkit_user_script_unref(script);
  }

  g_signal_connect(view, "decide-policy", G_CALLBACK(ao_decide_policy), NULL);
  g_signal_connect(view, "notify::title", G_CALLBACK(ao_notify_info), NULL);
  g_signal_connect(view, "notify::uri", G_CALLBACK(ao_notify_info), NULL);
  g_signal_connect(view, "load-changed", G_CALLBACK(ao_load_changed), NULL);
  g_signal_connect(view, "close", G_CALLBACK(ao_view_closed), NULL);
  g_signal_connect(view, "script-dialog", G_CALLBACK(ao_script_dialog), NULL);
  g_signal_connect(view, "run-file-chooser", G_CALLBACK(ao_file_chooser), NULL);
  g_signal_connect(view, "context-menu", G_CALLBACK(ao_context_menu), NULL);
  g_signal_connect(view, "create", G_CALLBACK(ao_create_view), NULL);
}

void *ao_wk_view_new(void *session, uint64_t page_id, const char *user_script,
                     const char *console_handler) {
  WebKitUserContentManager *ucm = webkit_user_content_manager_new();
  WebKitWebView *view = WEBKIT_WEB_VIEW(g_object_new(
      WEBKIT_TYPE_WEB_VIEW, "network-session", WEBKIT_NETWORK_SESSION(session),
      "user-content-manager", ucm, NULL));
  g_object_unref(ucm);
  if (view == NULL) {
    return NULL;
  }
  g_object_ref_sink(view);
  ao_connect_view(view, page_id, user_script, console_handler);
  return view;
}

void ao_wk_view_adopt(void *view, uint64_t page_id, const char *user_script,
                      const char *console_handler) {
  ao_connect_view(WEBKIT_WEB_VIEW(view), page_id, user_script, console_handler);
}

void ao_wk_view_close(void *view) {
  if (view == NULL) {
    return;
  }
  GtkWidget *w = GTK_WIDGET(view);
  ao_wk_host_unpark(view);
  if (ao_presented == w) {
    ao_presented = NULL;
  }
  webkit_web_view_try_close(WEBKIT_WEB_VIEW(view));
  g_object_unref(G_OBJECT(view));
}

void ao_wk_view_set_size(void *view, int width, int height) {
  gtk_widget_set_size_request(GTK_WIDGET(view), width, height);
}

void ao_wk_view_open_inspector(void *view) {
  WebKitWebInspector *inspector =
      webkit_web_view_get_inspector(WEBKIT_WEB_VIEW(view));
  if (inspector != NULL) {
    webkit_web_inspector_show(inspector);
  }
}

void ao_wk_view_load_uri(void *view, const char *uri) {
  webkit_web_view_load_uri(WEBKIT_WEB_VIEW(view), uri);
}

// action: 0 back, 1 forward, 2 reload, 3 stop.
void ao_wk_view_history(void *view, int action) {
  WebKitWebView *v = WEBKIT_WEB_VIEW(view);
  switch (action) {
    case 0:
      webkit_web_view_go_back(v);
      break;
    case 1:
      webkit_web_view_go_forward(v);
      break;
    case 2:
      webkit_web_view_reload(v);
      break;
    default:
      webkit_web_view_stop_loading(v);
      break;
  }
}

int ao_wk_view_is_loading(void *view) {
  return webkit_web_view_is_loading(WEBKIT_WEB_VIEW(view)) ? 1 : 0;
}

int ao_wk_view_can_go(void *view, int forward) {
  WebKitWebView *v = WEBKIT_WEB_VIEW(view);
  return (forward ? webkit_web_view_can_go_forward(v) : webkit_web_view_can_go_back(v)) ? 1 : 0;
}

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

static void ao_eval_finished(GObject *object, GAsyncResult *result, gpointer data) {
  uint64_t call_id = (uint64_t)GPOINTER_TO_SIZE(data);
  GError *error = NULL;
  JSCValue *value = webkit_web_view_call_async_javascript_function_finish(
      WEBKIT_WEB_VIEW(object), result, &error);
  if (error != NULL) {
    char *message = g_strdup(error->message ? error->message : "evaluation failed");
    g_error_free(error);
    aoWebKitEvalDone(call_id, NULL, message);
    return;
  }
  if (value == NULL) {
    aoWebKitEvalDone(call_id, NULL, NULL);
    return;
  }
  // jsc_value_to_json renders undefined as NULL, which the Go side reads as an
  // absent result — the same thing CDP reports for a void expression.
  char *json = jsc_value_to_json(value, 0);
  g_object_unref(value);
  aoWebKitEvalDone(call_id, json, NULL);
}

void ao_wk_view_eval(void *view, const char *body, uint64_t call_id) {
  webkit_web_view_call_async_javascript_function(
      WEBKIT_WEB_VIEW(view), body, -1, NULL, NULL, NULL, NULL, ao_eval_finished,
      GSIZE_TO_POINTER((gsize)call_id));
}

// ---------------------------------------------------------------------------
// Snapshot
// ---------------------------------------------------------------------------

static void ao_snapshot_finished(GObject *object, GAsyncResult *result, gpointer data) {
  uint64_t call_id = (uint64_t)GPOINTER_TO_SIZE(data);
  GError *error = NULL;
  GdkTexture *texture =
      webkit_web_view_get_snapshot_finish(WEBKIT_WEB_VIEW(object), result, &error);
  if (error != NULL || texture == NULL) {
    char *message = g_strdup(error && error->message ? error->message : "snapshot failed");
    if (error != NULL) {
      g_error_free(error);
    }
    aoWebKitSnapshotDone(call_id, NULL, 0, 0, 0, message);
    return;
  }
  int width = gdk_texture_get_width(texture);
  int height = gdk_texture_get_height(texture);
  gsize stride = (gsize)width * 4;
  guchar *pixels = g_malloc0(stride * (gsize)height);
  gdk_texture_download(texture, pixels, stride);
  g_object_unref(texture);
  aoWebKitSnapshotDone(call_id, pixels, width, height, (int)stride, NULL);
}

void ao_wk_view_snapshot(void *view, int full_document, uint64_t call_id) {
  webkit_web_view_get_snapshot(
      WEBKIT_WEB_VIEW(view),
      full_document ? WEBKIT_SNAPSHOT_REGION_FULL_DOCUMENT : WEBKIT_SNAPSHOT_REGION_VISIBLE,
      WEBKIT_SNAPSHOT_OPTIONS_NONE, NULL, ao_snapshot_finished,
      GSIZE_TO_POINTER((gsize)call_id));
}

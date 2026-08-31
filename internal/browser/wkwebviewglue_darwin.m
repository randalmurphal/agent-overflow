//go:build darwin && cgo && !ios && !server && !nogui

#include "wkwebviewglue_darwin.h"

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <objc/runtime.h>
#include <Block.h>
#include <stdlib.h>
#include <string.h>

// Callbacks implemented in Go (wkwebview_cgo_darwin.go).
extern void aoWKVEvalDone(uint64_t call_id, char *json, char *err);
extern void aoWKVSnapshotDone(uint64_t call_id, void *pixels, int width, int height,
                              int stride, char *err);
extern void aoWKVAllow(uint64_t page_id, void *decision, char *uri);
extern void aoWKVConsole(uint64_t page_id, char *payload);
extern void aoWKVPageInfo(uint64_t page_id, char *uri, char *title);
extern void aoWKVPageClosed(uint64_t page_id);
extern void aoWKVPopup(uint64_t opener_page_id, uint64_t profile_id, void *view, char *uri);
extern int aoWKVPagePresented(uint64_t page_id);
extern void aoWKVDownloadStarted(uint64_t profile_id, uint64_t page_id, uint64_t download_id,
                                 void *download, char *uri, char *suggested);
extern void aoWKVDownloadFinished(uint64_t download_id, double received, int state, char *path);

// The download state vocabulary the Go seam uses (driver.go).
#define AO_DL_IN_PROGRESS 0
#define AO_DL_COMPLETED 1
#define AO_DL_CANCELED 2

// Association keys. The address is the key, so each needs storage of its own.
static const char kAOPageIDKey;
static const char kAOProfileIDKey;
static const char kAODownloadDirKey;
static const char kAODownloadIDKey;
static const char kAODownloadPageKey;
static const char kAODownloadProfileKey;
static const char kAODownloadPathKey;
static int kAOObserverContext;

// ao_dup copies a UTF-8 string into malloc'd memory the Go half owns from that
// point on and releases through ao_wkv_free.
static char *ao_dup(NSString *text) {
  if (text == nil) {
    return NULL;
  }
  const char *utf8 = [text UTF8String];
  return utf8 == NULL ? NULL : strdup(utf8);
}

static uint64_t ao_number(id value) {
  return value == nil ? 0 : (uint64_t)[(NSNumber *)value unsignedLongLongValue];
}

static uint64_t ao_view_page_id(id view) {
  return ao_number(objc_getAssociatedObject(view, &kAOPageIDKey));
}

static uint64_t ao_view_profile_id(id view) {
  return ao_number(objc_getAssociatedObject(view, &kAOProfileIDKey));
}

// ---------------------------------------------------------------------------
// Host: the view tree AO owns inside the Wails window.
//
//   NSWindow
//   └── contentView (Wails' plain NSView)
//       ├── park view : 1x1, layer-masked, BELOW everything  (hidden pages)
//       ├── the Wails SPA WKWebView (unchanged, still live)
//       └── the presented page, ABOVE everything
//
// Order matters: the park view is added beneath the existing subviews so the
// SPA paints over its 1px footprint and it can never take a click, and the
// presented page is added on top so it sits above the SPA.
//
// This is the WKWebView analogue of the Linux 1x1 clipping GtkScrolledWindow.
// An off-window WKWebView is the trap it avoids: WebKit only guarantees layout
// and snapshots for a view that is IN a window, so hidden pages stay in the
// window and are clipped away instead of being unparented.
// ---------------------------------------------------------------------------

static NSView *ao_host = nil;
static NSView *ao_park = nil;
static NSView *ao_presented = nil;

int ao_wkv_supported(void) {
  @autoreleasepool {
    NSOperatingSystemVersion floor = {11, 0, 0};
    return [[NSProcessInfo processInfo] isOperatingSystemAtLeastVersion:floor] ? 1 : 0;
  }
}

int ao_wkv_host_attach(void *ns_window) {
  @autoreleasepool {
    if (ao_host != nil) {
      return 1;
    }
    NSWindow *window = (NSWindow *)ns_window;
    NSView *content = [window contentView];
    if (content == nil) {
      return 0;
    }
    NSView *park = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, 1, 1)];
    [park setWantsLayer:YES];
    [[park layer] setMasksToBounds:YES];
    [park setAutoresizingMask:NSViewMaxXMargin | NSViewMaxYMargin];
    NSArray *existing = [content subviews];
    if ([existing count] > 0) {
      [content addSubview:park positioned:NSWindowBelow relativeTo:[existing objectAtIndex:0]];
    } else {
      [content addSubview:park];
    }
    // Both pointers outlive every page and are never released: they are the
    // process's one host, exactly like the Linux overlay.
    ao_host = [content retain];
    ao_park = park;
    return 1;
  }
}

void ao_wkv_host_park(void *view, int slot, int width, int height) {
  @autoreleasepool {
    if (ao_park == nil || view == NULL) {
      return;
    }
    NSView *v = (NSView *)view;
    // Slots are stacked vertically inside the 1x1 clip, so two parked pages
    // never overlap and each keeps a viewport of its own.
    [v setFrame:NSMakeRect(0, (CGFloat)slot * (CGFloat)(height + 10), (CGFloat)width,
                           (CGFloat)height)];
    [v setHidden:NO];
    if ([v superview] == ao_park) {
      return;
    }
    ao_wkv_host_unpark(view);
    [ao_park addSubview:v];
  }
}

void ao_wkv_host_unpark(void *view) {
  @autoreleasepool {
    if (view == NULL) {
      return;
    }
    NSView *v = (NSView *)view;
    NSView *parent = [v superview];
    if (parent != nil && (parent == ao_park || parent == ao_host)) {
      [v removeFromSuperview];
    }
  }
}

void ao_wkv_host_present(void *view, double x, double y, double width,
                         double height, double vw, double vh) {
  @autoreleasepool {
    if (ao_host == nil || view == NULL) {
      return;
    }
    NSView *v = (NSView *)view;
    if (ao_presented != nil && ao_presented != v) {
      // Backstop only — the Manager hides every other page before it shows
      // one. Clearing the pointer is not enough here: the evicted view is
      // still a full-size subview of the host, so it must stop painting too.
      NSView *evicted = ao_presented;
      ao_presented = nil;
      [evicted setHidden:YES];
    }
    if ([v superview] != ao_host) {
      ao_wkv_host_unpark(view);
      [ao_host addSubview:v positioned:NSWindowAbove relativeTo:nil];
    }
    NSRect bounds = [ao_host bounds];
    // CSS pixels -> host points by proportion (see header). The host view and
    // the SPA viewport cover the same window, so the ratio carries both the
    // backing scale and any webview zoom.
    CGFloat sx = (vw > 0.0 && bounds.size.width > 0.0)
                     ? bounds.size.width / (CGFloat)vw
                     : 1.0;
    CGFloat sy = (vh > 0.0 && bounds.size.height > 0.0)
                     ? bounds.size.height / (CGFloat)vh
                     : 1.0;
    CGFloat fx = (CGFloat)x * sx;
    CGFloat fy = (CGFloat)y * sy;
    CGFloat fw = (CGFloat)width * sx;
    CGFloat fh = (CGFloat)height * sy;
    // The rect arrives in the SPA's top-left coordinates. AppKit's content view
    // is bottom-left unless it says otherwise, so the flip is asked for rather
    // than assumed.
    CGFloat originY = [ao_host isFlipped] ? fy : bounds.size.height - (fy + fh);
    [v setFrame:NSMakeRect(fx, originY, fw, fh)];
    [v setHidden:NO];
    ao_presented = v;
  }
}

void ao_wkv_host_hide(void *view) {
  if (view != NULL && ao_presented == (NSView *)view) {
    ao_presented = nil;
  }
}

int ao_wkv_host_presented(void *view) {
  return (view != NULL && ao_presented == (NSView *)view) ? 1 : 0;
}

// ---------------------------------------------------------------------------
// Delegates
//
// ONE shared delegate instance serves every view. Nothing per-page lives on it:
// a callback reads the page and profile ids off the web view itself, which is
// what keeps a popup whose configuration shares its opener's user content
// controller from reporting under the opener's identity.
// ---------------------------------------------------------------------------

@interface AOWebViewDelegate
    : NSObject <WKNavigationDelegate, WKUIDelegate, WKScriptMessageHandler>
@end

API_AVAILABLE(macos(11.3))
@interface AODownloadDelegate : NSObject <WKDownloadDelegate>
@end

static AOWebViewDelegate *ao_delegate(void) {
  static AOWebViewDelegate *shared = nil;
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    shared = [[AOWebViewDelegate alloc] init];
  });
  return shared;
}

API_AVAILABLE(macos(11.3))
static id ao_download_delegate(void) {
  static AODownloadDelegate *shared = nil;
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    shared = [[AODownloadDelegate alloc] init];
  });
  return shared;
}

static uint64_t ao_download_seq = 0;

// ao_attach_download stamps the four facts the download delegate needs onto the
// download itself: WKDownload exposes no route back to the store or the page,
// and its -webView is nil for a resumed one.
API_AVAILABLE(macos(11.3))
static void ao_attach_download(WKWebView *view, WKDownload *download) {
  uint64_t identifier = ++ao_download_seq;
  objc_setAssociatedObject(download, &kAODownloadIDKey, @(identifier),
                           OBJC_ASSOCIATION_RETAIN_NONATOMIC);
  objc_setAssociatedObject(download, &kAODownloadPageKey, @(ao_view_page_id(view)),
                           OBJC_ASSOCIATION_RETAIN_NONATOMIC);
  objc_setAssociatedObject(download, &kAODownloadProfileKey, @(ao_view_profile_id(view)),
                           OBJC_ASSOCIATION_RETAIN_NONATOMIC);
  objc_setAssociatedObject(download, &kAODownloadDirKey,
                           objc_getAssociatedObject(view, &kAODownloadDirKey),
                           OBJC_ASSOCIATION_RETAIN_NONATOMIC);
  // Held until the Go side releases it, so a cancel arriving after the page
  // closed cannot reach a freed object.
  [download retain];
  [download setDelegate:ao_download_delegate()];
}

@implementation AOWebViewDelegate

// ---- navigation authority ----

- (void)webView:(WKWebView *)webView
    decidePolicyForNavigationAction:(WKNavigationAction *)navigationAction
                    decisionHandler:(void (^)(WKNavigationActionPolicy))decisionHandler {
  NSURL *url = [[navigationAction request] URL];
  if (url == nil) {
    decisionHandler(WKNavigationActionPolicyCancel);
    return;
  }
  // The decision is DEFERRED, not answered here: navigation authority is the
  // Manager's, and asking it takes Manager locks. Blocking the main thread on a
  // Go lock is how the whole UI freezes behind one browser operation, so the
  // handler block is copied and finished from a goroutine.
  void (^held)(WKNavigationActionPolicy) = Block_copy(decisionHandler);
  aoWKVAllow(ao_view_page_id(webView), (void *)held, ao_dup([url absoluteString]));
}

- (void)webView:(WKWebView *)webView
    decidePolicyForNavigationResponse:(WKNavigationResponse *)navigationResponse
                      decisionHandler:(void (^)(WKNavigationResponsePolicy))decisionHandler {
  if (@available(macOS 11.3, *)) {
    if (![navigationResponse canShowMIMEType]) {
      decisionHandler(WKNavigationResponsePolicyDownload);
      return;
    }
  }
  decisionHandler(WKNavigationResponsePolicyAllow);
}

- (void)webView:(WKWebView *)webView
      navigationAction:(WKNavigationAction *)navigationAction
     didBecomeDownload:(WKDownload *)download API_AVAILABLE(macos(11.3)) {
  ao_attach_download(webView, download);
}

- (void)webView:(WKWebView *)webView
      navigationResponse:(WKNavigationResponse *)navigationResponse
       didBecomeDownload:(WKDownload *)download API_AVAILABLE(macos(11.3)) {
  ao_attach_download(webView, download);
}

// ---- page identity ----

- (void)observeValueForKeyPath:(NSString *)keyPath
                      ofObject:(id)object
                        change:(NSDictionary *)change
                       context:(void *)context {
  if (context != &kAOObserverContext) {
    [super observeValueForKeyPath:keyPath ofObject:object change:change context:context];
    return;
  }
  WKWebView *view = (WKWebView *)object;
  aoWKVPageInfo(ao_view_page_id(view), ao_dup([[view URL] absoluteString]), ao_dup([view title]));
}

// ---- popups ----

- (WKWebView *)webView:(WKWebView *)webView
    createWebViewWithConfiguration:(WKWebViewConfiguration *)configuration
               forNavigationAction:(WKNavigationAction *)navigationAction
                    windowFeatures:(WKWindowFeatures *)windowFeatures {
  // The configuration WebKit hands over carries the opener's data store and
  // process, which is what makes the popup part of the same workspace profile.
  // It is never replaced.
  WKWebView *popup = [[WKWebView alloc] initWithFrame:NSMakeRect(0, 0, 1280, 800)
                                        configuration:configuration];
  NSURL *url = [[navigationAction request] URL];
  // Held (the +1 from -alloc) until the Manager adopts or discards it. WebKit
  // does not retain the view it is handed back.
  aoWKVPopup(ao_view_page_id(webView), ao_view_profile_id(webView), popup,
             ao_dup([url absoluteString]));
  return popup;
}

- (void)webViewDidClose:(WKWebView *)webView {
  aoWKVPageClosed(ao_view_page_id(webView));
}

// ---- dialogs ----
//
// WKWebView ships no dialogs of its own: an unimplemented delegate method makes
// the JavaScript call return immediately. So the hidden-page answer (dismiss,
// which is what the CDP driver does) and the presented-page answer (a real
// panel, which is what a user looking at the page expects) are both spelled
// out here.

- (void)webView:(WKWebView *)webView
    runJavaScriptAlertPanelWithMessage:(NSString *)message
                      initiatedByFrame:(WKFrameInfo *)frame
                     completionHandler:(void (^)(void))completionHandler {
  if (!aoWKVPagePresented(ao_view_page_id(webView))) {
    completionHandler();
    return;
  }
  NSAlert *alert = [[[NSAlert alloc] init] autorelease];
  [alert setMessageText:message == nil ? @"" : message];
  [alert addButtonWithTitle:@"OK"];
  NSWindow *window = [webView window];
  void (^held)(void) = Block_copy(completionHandler);
  if (window == nil) {
    [alert runModal];
    held();
    Block_release(held);
    return;
  }
  [alert beginSheetModalForWindow:window
                completionHandler:^(NSModalResponse response) {
                  (void)response;
                  held();
                  Block_release(held);
                }];
}

- (void)webView:(WKWebView *)webView
    runJavaScriptConfirmPanelWithMessage:(NSString *)message
                        initiatedByFrame:(WKFrameInfo *)frame
                       completionHandler:(void (^)(BOOL))completionHandler {
  if (!aoWKVPagePresented(ao_view_page_id(webView))) {
    completionHandler(NO);
    return;
  }
  NSAlert *alert = [[[NSAlert alloc] init] autorelease];
  [alert setMessageText:message == nil ? @"" : message];
  [alert addButtonWithTitle:@"OK"];
  [alert addButtonWithTitle:@"Cancel"];
  NSWindow *window = [webView window];
  void (^held)(BOOL) = Block_copy(completionHandler);
  if (window == nil) {
    held([alert runModal] == NSAlertFirstButtonReturn);
    Block_release(held);
    return;
  }
  [alert beginSheetModalForWindow:window
                completionHandler:^(NSModalResponse response) {
                  held(response == NSAlertFirstButtonReturn);
                  Block_release(held);
                }];
}

- (void)webView:(WKWebView *)webView
    runJavaScriptTextInputPanelWithPrompt:(NSString *)prompt
                              defaultText:(NSString *)defaultText
                         initiatedByFrame:(WKFrameInfo *)frame
                        completionHandler:(void (^)(NSString *))completionHandler {
  if (!aoWKVPagePresented(ao_view_page_id(webView))) {
    completionHandler(nil);
    return;
  }
  NSAlert *alert = [[[NSAlert alloc] init] autorelease];
  [alert setMessageText:prompt == nil ? @"" : prompt];
  [alert addButtonWithTitle:@"OK"];
  [alert addButtonWithTitle:@"Cancel"];
  NSTextField *input =
      [[[NSTextField alloc] initWithFrame:NSMakeRect(0, 0, 260, 24)] autorelease];
  [input setStringValue:defaultText == nil ? @"" : defaultText];
  [alert setAccessoryView:input];
  NSWindow *window = [webView window];
  void (^held)(NSString *) = Block_copy(completionHandler);
  if (window == nil) {
    NSModalResponse response = [alert runModal];
    held(response == NSAlertFirstButtonReturn ? [input stringValue] : nil);
    Block_release(held);
    return;
  }
  [alert beginSheetModalForWindow:window
                completionHandler:^(NSModalResponse response) {
                  held(response == NSAlertFirstButtonReturn ? [input stringValue] : nil);
                  Block_release(held);
                }];
}

// ---- file chooser ----
//
// A hidden agent page has nobody to answer a picker, so it is refused. A
// presented page gets the real NSOpenPanel — WKWebView has no built-in one, so
// not implementing this method would leave the file input inert instead.

- (void)webView:(WKWebView *)webView
    runOpenPanelWithParameters:(WKOpenPanelParameters *)parameters
              initiatedByFrame:(WKFrameInfo *)frame
             completionHandler:(void (^)(NSArray<NSURL *> *))completionHandler {
  if (!aoWKVPagePresented(ao_view_page_id(webView))) {
    completionHandler(nil);
    return;
  }
  NSOpenPanel *panel = [NSOpenPanel openPanel];
  [panel setAllowsMultipleSelection:[parameters allowsMultipleSelection]];
  [panel setCanChooseFiles:YES];
  [panel setCanChooseDirectories:NO];
  void (^held)(NSArray<NSURL *> *) = Block_copy(completionHandler);
  void (^done)(NSModalResponse) = ^(NSModalResponse response) {
    held(response == NSModalResponseOK ? [panel URLs] : nil);
    Block_release(held);
  };
  NSWindow *window = [webView window];
  if (window == nil) {
    [panel beginWithCompletionHandler:done];
  } else {
    [panel beginSheetModalForWindow:window completionHandler:done];
  }
}

// ---- console ----

- (void)userContentController:(WKUserContentController *)controller
      didReceiveScriptMessage:(WKScriptMessage *)message {
  if (![[message body] isKindOfClass:[NSString class]]) {
    return;
  }
  WKWebView *view = [message webView];
  if (view == nil) {
    return;
  }
  uint64_t page_id = ao_view_page_id(view);
  if (page_id == 0) {
    return;
  }
  aoWKVConsole(page_id, ao_dup((NSString *)[message body]));
}

@end

@implementation AODownloadDelegate

- (void)download:(WKDownload *)download
    decideDestinationUsingResponse:(NSURLResponse *)response
                 suggestedFilename:(NSString *)suggestedFilename
                 completionHandler:(void (^)(NSURL *))completionHandler {
  NSString *dir = objc_getAssociatedObject(download, &kAODownloadDirKey);
  if (dir == nil || [dir length] == 0) {
    // A nil destination cancels the download, which is the only right answer
    // for a profile with no AO artifact directory to write into.
    completionHandler(nil);
    return;
  }
  uint64_t identifier = ao_number(objc_getAssociatedObject(download, &kAODownloadIDKey));
  // The handle IS the on-disk name. The Manager renames the finished file to a
  // sanitized unique name inside the same directory; giving it a handle-named
  // file also lets it clean up a download it had to cancel mid-flight.
  NSString *path =
      [dir stringByAppendingPathComponent:[NSString stringWithFormat:@"dl-%llu",
                                                                    (unsigned long long)identifier]];
  // WebKit refuses a destination that already exists.
  [[NSFileManager defaultManager] removeItemAtPath:path error:nil];
  objc_setAssociatedObject(download, &kAODownloadPathKey, path, OBJC_ASSOCIATION_RETAIN_NONATOMIC);
  completionHandler([NSURL fileURLWithPath:path]);
  NSURL *url = [[download originalRequest] URL];
  aoWKVDownloadStarted(ao_number(objc_getAssociatedObject(download, &kAODownloadProfileKey)),
                       ao_number(objc_getAssociatedObject(download, &kAODownloadPageKey)),
                       identifier, download, ao_dup([url absoluteString]),
                       ao_dup(suggestedFilename));
}

- (void)downloadDidFinish:(WKDownload *)download {
  NSString *path = objc_getAssociatedObject(download, &kAODownloadPathKey);
  aoWKVDownloadFinished(ao_number(objc_getAssociatedObject(download, &kAODownloadIDKey)),
                        (double)[[download progress] completedUnitCount], AO_DL_COMPLETED,
                        ao_dup(path));
}

- (void)download:(WKDownload *)download
    didFailWithError:(NSError *)error
          resumeData:(NSData *)resumeData {
  aoWKVDownloadFinished(ao_number(objc_getAssociatedObject(download, &kAODownloadIDKey)),
                        (double)[[download progress] completedUnitCount], AO_DL_CANCELED, NULL);
}

@end

void ao_wkv_policy_finish(void *decision, int allow) {
  @autoreleasepool {
    if (decision == NULL) {
      return;
    }
    void (^held)(WKNavigationActionPolicy) = (void (^)(WKNavigationActionPolicy))decision;
    held(allow ? WKNavigationActionPolicyAllow : WKNavigationActionPolicyCancel);
    Block_release(held);
  }
}

void ao_wkv_free(void *pointer) { free(pointer); }

// ---------------------------------------------------------------------------
// Website data store
// ---------------------------------------------------------------------------

void *ao_wkv_store_new(const char *identifier_uuid, int ephemeral) {
  @autoreleasepool {
    if (!ephemeral && identifier_uuid != NULL) {
      if (@available(macOS 14.0, *)) {
        NSUUID *identifier =
            [[[NSUUID alloc] initWithUUIDString:[NSString stringWithUTF8String:identifier_uuid]]
                autorelease];
        if (identifier != nil) {
          WKWebsiteDataStore *store = [WKWebsiteDataStore dataStoreForIdentifier:identifier];
          if (store != nil) {
            return [store retain];
          }
        }
      }
    }
    // Everything else — the site-data setting off, or a macOS with no API for a
    // per-workspace persistent store — is in-memory only, which is the honest
    // meaning of "this engine cannot persist that here".
    return [[WKWebsiteDataStore nonPersistentDataStore] retain];
  }
}

void ao_wkv_store_free(void *store) {
  @autoreleasepool {
    if (store != NULL) {
      [(WKWebsiteDataStore *)store release];
    }
  }
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

static void ao_connect_view(WKWebView *view, uint64_t page_id, uint64_t profile_id,
                            const char *user_script, const char *console_handler,
                            const char *download_dir) {
  objc_setAssociatedObject(view, &kAOPageIDKey, @(page_id), OBJC_ASSOCIATION_RETAIN_NONATOMIC);
  objc_setAssociatedObject(view, &kAOProfileIDKey, @(profile_id),
                           OBJC_ASSOCIATION_RETAIN_NONATOMIC);
  objc_setAssociatedObject(
      view, &kAODownloadDirKey,
      download_dir == NULL ? nil : [NSString stringWithUTF8String:download_dir],
      OBJC_ASSOCIATION_RETAIN_NONATOMIC);

  WKUserContentController *controller = [[view configuration] userContentController];
  if (controller != nil && console_handler != NULL) {
    NSString *name = [NSString stringWithUTF8String:console_handler];
    // A popup's configuration may already carry the opener's controller, and
    // adding a handler name twice RAISES. Removing first would leave a window
    // in which the opener's live page posts into nothing, so the duplicate is
    // caught instead — the handler being added is the same shared object.
    @try {
      [controller addScriptMessageHandler:ao_delegate() name:name];
    } @catch (NSException *ignored) {
      (void)ignored;
    }
    // The same controller may already carry the opener's script; adding it
    // again would double every console entry the opener reports.
    if (user_script != NULL && [[controller userScripts] count] == 0) {
      WKUserScript *script = [[[WKUserScript alloc]
             initWithSource:[NSString stringWithUTF8String:user_script]
              injectionTime:WKUserScriptInjectionTimeAtDocumentStart
           forMainFrameOnly:NO] autorelease];
      [controller addUserScript:script];
    }
  }

  [view setNavigationDelegate:ao_delegate()];
  [view setUIDelegate:ao_delegate()];
  // Title and URL change without a navigation on any single-page app, so both
  // are observed rather than read off the navigation callbacks.
  [view addObserver:ao_delegate()
         forKeyPath:@"title"
            options:NSKeyValueObservingOptionNew
            context:&kAOObserverContext];
  [view addObserver:ao_delegate()
         forKeyPath:@"URL"
            options:NSKeyValueObservingOptionNew
            context:&kAOObserverContext];
  // Spec §7: macOS devtools are Safari's Develop menu against an inspectable
  // view, not an in-app inspector.
  if (@available(macOS 13.3, *)) {
    [view setInspectable:YES];
  }
}

void *ao_wkv_view_new(void *store, uint64_t page_id, uint64_t profile_id,
                      const char *user_script, const char *console_handler,
                      const char *download_dir) {
  @autoreleasepool {
    WKWebViewConfiguration *configuration = [[[WKWebViewConfiguration alloc] init] autorelease];
    if (store != NULL) {
      [configuration setWebsiteDataStore:(WKWebsiteDataStore *)store];
    }
    [[configuration preferences] setJavaScriptCanOpenWindowsAutomatically:YES];
    WKUserContentController *controller = [[[WKUserContentController alloc] init] autorelease];
    [configuration setUserContentController:controller];
    WKWebView *view = [[WKWebView alloc] initWithFrame:NSMakeRect(0, 0, 1280, 800)
                                         configuration:configuration];
    if (view == nil) {
      return NULL;
    }
    ao_connect_view(view, page_id, profile_id, user_script, console_handler, download_dir);
    return view;
  }
}

void ao_wkv_view_adopt(void *view, uint64_t page_id, uint64_t profile_id,
                       const char *user_script, const char *console_handler,
                       const char *download_dir) {
  @autoreleasepool {
    if (view != NULL) {
      ao_connect_view((WKWebView *)view, page_id, profile_id, user_script, console_handler,
                      download_dir);
    }
  }
}

void ao_wkv_view_close(void *view) {
  @autoreleasepool {
    if (view == NULL) {
      return;
    }
    WKWebView *v = (WKWebView *)view;
    if (ao_presented == (NSView *)v) {
      ao_presented = nil;
    }
    ao_wkv_host_unpark(view);
    [v stopLoading];
    [v setNavigationDelegate:nil];
    [v setUIDelegate:nil];
    // An observer left registered on a deallocating object is a hard crash, and
    // a view that never reached ao_connect_view has none.
    @try {
      [v removeObserver:ao_delegate() forKeyPath:@"title" context:&kAOObserverContext];
      [v removeObserver:ao_delegate() forKeyPath:@"URL" context:&kAOObserverContext];
    } @catch (NSException *ignored) {
      (void)ignored;
    }
    objc_setAssociatedObject(v, &kAOPageIDKey, nil, OBJC_ASSOCIATION_RETAIN_NONATOMIC);
    [v release];
  }
}

void ao_wkv_view_set_size(void *view, int width, int height) {
  @autoreleasepool {
    if (view == NULL) {
      return;
    }
    NSRect frame = [(NSView *)view frame];
    frame.size = NSMakeSize((CGFloat)width, (CGFloat)height);
    [(NSView *)view setFrame:frame];
  }
}

void ao_wkv_view_get_size(void *view, int *width, int *height) {
  @autoreleasepool {
    if (view == NULL) {
      return;
    }
    NSRect frame = [(NSView *)view frame];
    if (width != NULL) {
      *width = (int)frame.size.width;
    }
    if (height != NULL) {
      *height = (int)frame.size.height;
    }
  }
}

void ao_wkv_view_load_uri(void *view, const char *uri) {
  @autoreleasepool {
    if (view == NULL || uri == NULL) {
      return;
    }
    NSURL *url = [NSURL URLWithString:[NSString stringWithUTF8String:uri]];
    if (url == nil) {
      return;
    }
    [(WKWebView *)view loadRequest:[NSURLRequest requestWithURL:url]];
  }
}

void ao_wkv_view_load_file(void *view, const char *path, const char *read_access_dir) {
  @autoreleasepool {
    if (view == NULL || path == NULL || read_access_dir == NULL) {
      return;
    }
    NSURL *file = [NSURL fileURLWithPath:[NSString stringWithUTF8String:path]];
    NSURL *root = [NSURL fileURLWithPath:[NSString stringWithUTF8String:read_access_dir]
                             isDirectory:YES];
    [(WKWebView *)view loadFileURL:file allowingReadAccessToURL:root];
  }
}

// action: 0 back, 1 forward, 2 reload, 3 stop.
void ao_wkv_view_history(void *view, int action) {
  @autoreleasepool {
    if (view == NULL) {
      return;
    }
    WKWebView *v = (WKWebView *)view;
    switch (action) {
      case 0:
        [v goBack];
        break;
      case 1:
        [v goForward];
        break;
      case 2:
        [v reload];
        break;
      default:
        [v stopLoading];
        break;
    }
  }
}

int ao_wkv_view_is_loading(void *view) {
  @autoreleasepool {
    return (view != NULL && [(WKWebView *)view isLoading]) ? 1 : 0;
  }
}

int ao_wkv_view_can_go(void *view, int forward) {
  @autoreleasepool {
    if (view == NULL) {
      return 0;
    }
    WKWebView *v = (WKWebView *)view;
    return (forward ? [v canGoForward] : [v canGoBack]) ? 1 : 0;
  }
}

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

void ao_wkv_view_eval(void *view, const char *body, uint64_t call_id) {
  @autoreleasepool {
    if (view == NULL || body == NULL) {
      aoWKVEvalDone(call_id, NULL, strdup("browser: the page is gone"));
      return;
    }
    if (@available(macOS 11.0, *)) {
      // -callAsyncJavaScript: takes an async FUNCTION BODY and awaits its
      // return value, which is exactly what WebKitGTK's
      // webkit_web_view_call_async_javascript_function does. -evaluateJavaScript
      // would hand back an unresolved promise instead.
      [(WKWebView *)view callAsyncJavaScript:[NSString stringWithUTF8String:body]
                                   arguments:nil
                                     inFrame:nil
                              inContentWorld:[WKContentWorld pageWorld]
                           completionHandler:^(id result, NSError *error) {
                             if (error != nil) {
                               aoWKVEvalDone(call_id, NULL, ao_dup([error localizedDescription]));
                               return;
                             }
                             if (result == nil) {
                               // undefined: an absent result, the same thing CDP
                               // reports for a void expression.
                               aoWKVEvalDone(call_id, NULL, NULL);
                               return;
                             }
                             NSError *encodeError = nil;
                             NSData *data = [NSJSONSerialization
                                 dataWithJSONObject:result
                                            options:NSJSONWritingFragmentsAllowed
                                              error:&encodeError];
                             if (data == nil) {
                               aoWKVEvalDone(call_id, NULL,
                                             strdup("browser: result is not JSON-encodable"));
                               return;
                             }
                             NSString *json = [[[NSString alloc]
                                 initWithData:data
                                     encoding:NSUTF8StringEncoding] autorelease];
                             aoWKVEvalDone(call_id, ao_dup(json), NULL);
                           }];
      return;
    }
    aoWKVEvalDone(call_id, NULL, strdup("browser: this engine needs macOS 11 or newer"));
  }
}

// ---------------------------------------------------------------------------
// Snapshot
// ---------------------------------------------------------------------------

// ao_snapshot_deliver normalizes whatever WebKit rendered to ONE image pixel per
// CSS pixel, premultiplied BGRA — the same buffer shape the GTK engine hands
// over, so webkitimage.go decodes and crops both without knowing which engine
// produced them. Clip rects arrive in page coordinates, so a backing-scaled
// image would crop at the wrong place; HiDPI crispness is deliberately traded
// for that.
static void ao_snapshot_deliver(uint64_t call_id, NSImage *image, int width, int height) {
  CGImageRef cg = image == nil ? NULL : [image CGImageForProposedRect:NULL context:nil hints:nil];
  if (cg == NULL || width <= 0 || height <= 0) {
    aoWKVSnapshotDone(call_id, NULL, 0, 0, 0, strdup("browser: screenshot returned no pixels"));
    return;
  }
  size_t stride = (size_t)width * 4;
  void *pixels = calloc(stride, (size_t)height);
  if (pixels == NULL) {
    aoWKVSnapshotDone(call_id, NULL, 0, 0, 0, strdup("browser: screenshot allocation failed"));
    return;
  }
  CGColorSpaceRef space = CGColorSpaceCreateDeviceRGB();
  CGContextRef ctx = CGBitmapContextCreate(pixels, (size_t)width, (size_t)height, 8, stride, space,
                                           kCGImageAlphaPremultipliedFirst |
                                               kCGBitmapByteOrder32Little);
  CGColorSpaceRelease(space);
  if (ctx == NULL) {
    free(pixels);
    aoWKVSnapshotDone(call_id, NULL, 0, 0, 0, strdup("browser: screenshot context failed"));
    return;
  }
  // A bitmap context's origin is bottom-left; flipping makes row 0 the top row,
  // which is what a top-down RGBA image means everywhere else.
  CGContextTranslateCTM(ctx, 0, (CGFloat)height);
  CGContextScaleCTM(ctx, 1.0, -1.0);
  CGContextDrawImage(ctx, CGRectMake(0, 0, (CGFloat)width, (CGFloat)height), cg);
  CGContextRelease(ctx);
  aoWKVSnapshotDone(call_id, pixels, width, height, (int)stride, NULL);
}

void ao_wkv_view_snapshot(void *view, uint64_t call_id) {
  @autoreleasepool {
    if (view == NULL) {
      aoWKVSnapshotDone(call_id, NULL, 0, 0, 0, strdup("browser: the page is gone"));
      return;
    }
    WKWebView *v = (WKWebView *)view;
    NSRect bounds = [v bounds];
    int width = (int)bounds.size.width;
    int height = (int)bounds.size.height;
    WKSnapshotConfiguration *config = [[[WKSnapshotConfiguration alloc] init] autorelease];
    [config setAfterScreenUpdates:YES];
    [config setSnapshotWidth:@(width)];
    [v takeSnapshotWithConfiguration:config
                   completionHandler:^(NSImage *image, NSError *error) {
                     if (error != nil) {
                       aoWKVSnapshotDone(call_id, NULL, 0, 0, 0,
                                         ao_dup([error localizedDescription]));
                       return;
                     }
                     ao_snapshot_deliver(call_id, image, width, height);
                   }];
  }
}

// ---------------------------------------------------------------------------
// Downloads
// ---------------------------------------------------------------------------

void ao_wkv_download_cancel(void *download) {
  @autoreleasepool {
    if (download == NULL) {
      return;
    }
    if (@available(macOS 11.3, *)) {
      [(WKDownload *)download cancel:nil];
    }
  }
}

void ao_wkv_download_release(void *download) {
  @autoreleasepool {
    if (download != NULL) {
      [(id)download release];
    }
  }
}

double ao_wkv_download_received(void *download) {
  @autoreleasepool {
    if (download == NULL) {
      return 0;
    }
    if (@available(macOS 11.3, *)) {
      return (double)[[(WKDownload *)download progress] completedUnitCount];
    }
    return 0;
  }
}

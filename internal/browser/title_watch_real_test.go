//go:build !windows

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// titleWatchCases are documents whose title changes in ways a watcher
// started at DOMContentLoaded, observing the head it found then, misses or
// never sees: each serves its document at its path and wants the titles
// in order.
var titleWatchCases = []struct {
	name, path, contentType, body string
	want                          []string
}{
	{
		name: "document.write replaces the document", path: "/write", contentType: "text/html",
		body: `<!doctype html><title>Parsed</title><script>
setTimeout(() => {
  document.open();
  document.write("<!doctype html><title>Rewritten</title><script>setTimeout(() => { document.title = 'After the rewrite' }, 300)<\/script>");
  document.close();
}, 300)
</script>`,
		want: []string{"Parsed", "Rewritten", "After the rewrite"},
	},
	{
		name: "the root element is replaced", path: "/root", contentType: "text/html",
		body: `<!doctype html><title>Parsed</title><script>
setTimeout(() => {
  const html = document.createElement("html"), head = document.createElement("head"), title = document.createElement("title");
  title.textContent = "Replaced";
  head.append(title);
  html.append(head);
  document.replaceChild(html, document.documentElement);
  setTimeout(() => { title.textContent = "Replaced again" }, 300);
}, 300)
</script>`,
		want: []string{"Parsed", "Replaced", "Replaced again"},
	},
	{
		name: "an SVG document's title changes", path: "/svg", contentType: "image/svg+xml",
		body: `<svg xmlns="http://www.w3.org/2000/svg"><title>Drawing</title><script>
setTimeout(() => { document.querySelector("title").textContent = "Drawing changed" }, 300)
</script></svg>`,
		want: []string{"Drawing", "Drawing changed"},
	},
	{
		name: "a capture listener on the document stops DOMContentLoaded", path: "/capture-document", contentType: "text/html",
		body: `<!doctype html><title>Parsed</title><script>
document.addEventListener("DOMContentLoaded", (event) => event.stopImmediatePropagation(), true)
</script>`,
		want: []string{"Parsed"},
	},
	{
		name: "a capture listener on the window stops DOMContentLoaded", path: "/capture-window", contentType: "text/html",
		body: `<!doctype html><title>Parsed</title><script>
window.addEventListener("DOMContentLoaded", (event) => event.stopImmediatePropagation(), true)
</script>`,
		want: []string{"Parsed"},
	},
	{
		// XHTML creates no head of its own: the title is parsed as the
		// root's child before the head, and document.title reads it there.
		name: "a title set before the head exists", path: "/before-head", contentType: "application/xhtml+xml",
		body: `<html xmlns="http://www.w3.org/1999/xhtml"><title>Early</title><head></head><body><script>
setTimeout(() => { document.querySelector("title").textContent = "Early changed" }, 300)
</script></body></html>`,
		want: []string{"Early", "Early changed"},
	},
	{
		// document.title reads the first title in tree order, which is now
		// the root's child before the head, not the head's.
		name: "a title is put before the head's", path: "/before-head-title", contentType: "text/html",
		body: `<!doctype html><title>Head title</title><script>
setTimeout(() => {
  const title = document.createElement("title");
  title.textContent = "Root title";
  document.documentElement.prepend(title);
  setTimeout(() => { title.textContent = "Root changed" }, 300);
}, 300)
</script>`,
		want: []string{"Head title", "Root title", "Root changed"},
	},
	{
		name: "the head's title is replaced by one in the body", path: "/body-title", contentType: "text/html",
		body: `<!doctype html><title>Head title</title><body><div id="d"></div><script>
setTimeout(() => {
  document.querySelector("title").remove();
  const title = document.createElement("title");
  title.textContent = "Body title";
  document.getElementById("d").append(title);
  setTimeout(() => { title.textContent = "Body changed" }, 300);
}, 300)
</script></body>`,
		want: []string{"Head title", "Body title", "Body changed"},
	},
	{
		// The first script ends the parse's first observed mutations before
		// the body's title is parsed, so the report is what finds it.
		name: "a title parsed into the body", path: "/parsed-body-title", contentType: "text/html",
		body: `<!doctype html><body><script>0</script><div><title>Body title</title></div><script>
setTimeout(() => { document.querySelector("title").textContent = "Body changed" }, 300)
</script></body>`,
		want: []string{"Body title", "Body changed"},
	},
	{
		// An inline SVG's title comes first in tree order but is not an
		// HTML title element: the body's is the document's.
		name: "an inline SVG title is not the document's", path: "/inline-svg", contentType: "text/html",
		body: `<!doctype html><body><svg><title>Icon</title></svg><div><title>Body title</title></div><script>
setTimeout(() => { document.querySelector("div title").textContent = "Body changed" }, 300)
</script></body>`,
		want: []string{"Body title", "Body changed"},
	},
}

// lateBody is the watcher's stated limit: a title inserted into the body
// after load while nothing the watcher observes changes.
const lateBody = `<!doctype html><body><div id="d"></div><script>
setTimeout(() => {
  const title = document.createElement("title");
  title.textContent = "Late";
  document.getElementById("d").append(title);
  window.inserted = true;
}, 300)
</script></body>`

// headChildInsertions is how many head children the untitled document
// inserts, each delivered on its own, over a body of bodyElements.
const (
	headChildInsertions = 1000
	bodyElements        = 5000
)

// untitledHead inserts head children one per microtask into an untitled
// document whose body is large, so each one costs the watcher a callback
// and a title lookup over the whole document, and records how long the
// insertions took.
var untitledHead = fmt.Sprintf(`<!doctype html><body><script>
for (let i = 0; i < %d; i++) document.body.append(document.createElement("p"));
setTimeout(async () => {
  const start = performance.now();
  for (let i = 0; i < %d; i++) {
    document.head.append(document.createElement("meta"));
    await null;
  }
  window.elapsed = performance.now() - start;
}, 300)
</script></body>`, bodyElements, headChildInsertions)

// busyBodyMutations is how many separately delivered body mutations the
// busy document makes before it retitles itself.
const busyBodyMutations = 10000

// busyBody mutates its body once per microtask, so a watcher observing the
// body would run a callback for each, and then sets its title.
const busyBody = `<!doctype html><title>Busy</title><body><script>
setTimeout(async () => {
  for (let i = 0; i < 10000; i++) {
    document.body.append(document.createElement("i"));
    await null;
  }
  document.title = "Busy done";
}, 300)
</script></body>`

// assertRealDocumentTitleChanges proves on a real Chromium that the title
// watcher follows every way a document's title changes (titleWatchCases),
// that a page cannot keep it from starting, and that a busy body costs it
// no callbacks.
func assertRealDocumentTitleChanges(t *testing.T, engine *headlessEngine) {
	t.Helper()
	mux := http.NewServeMux()
	for _, tc := range titleWatchCases {
		mux.HandleFunc(tc.path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", tc.contentType)
			_, _ = w.Write([]byte(tc.body))
		})
	}
	for path, body := range map[string]string{"/busy": busyBody, "/late": lateBody, "/untitled-head": untitledHead} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(body))
		})
	}
	server := httptest.NewServer(mux)
	defer server.Close()
	downloads := filepath.Join(t.TempDir(), "downloads")
	created, err := engine.NewProfile(t.Context(), profileOptions{
		Jar: jarKey(t.TempDir()), DownloadDir: downloads, OpenDownloadDir: openDirByName(downloads), Ephemeral: true, Allow: func([]frameID, string) (bool, bool) { return true, true },
	})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	profile := created.(*headlessProfile)
	defer func() {
		if err := profile.Dispose(context.Background()); err != nil {
			t.Errorf("dispose: %v", err)
		}
	}()
	// open loads path in a new page and answers the page and the titles
	// its document reports at that address.
	open := func(t *testing.T, path string) (*cdpPage, chan string) {
		t.Helper()
		titles := make(chan string, 16)
		hooks := testPageHooks()
		hooks.DocumentTitle = func(url, title string) {
			if url == server.URL+path {
				titles <- title
			}
		}
		driver, err := profile.NewPage(t.Context(), hooks)
		if err != nil {
			t.Fatalf("new page: %v", err)
		}
		t.Cleanup(driver.Close)
		opCtx, cancel := operationContext(t.Context(), driver.Lifetime(), operationTimeout)
		defer cancel()
		if err := driver.Navigate(opCtx, server.URL+path); err != nil {
			t.Fatalf("navigate: %v", err)
		}
		return driver.(*cdpPage), titles
	}
	expect := func(t *testing.T, titles chan string, want ...string) {
		t.Helper()
		for _, title := range want {
			select {
			case got := <-titles:
				if got != title {
					t.Fatalf("the document reported %q, want %q", got, title)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("the document never reported %q", title)
			}
		}
	}
	for _, tc := range titleWatchCases {
		t.Run(tc.name, func(t *testing.T) {
			_, titles := open(t, tc.path)
			expect(t, titles, tc.want...)
		})
	}
	t.Run("a busy body costs no callbacks", func(t *testing.T) {
		p, titles := open(t, "/busy")
		expect(t, titles, "Busy", "Busy done")
		callbacks := titleWatchCallbacks(t, p)
		// The parse and the retitle are a handful; one per body mutation
		// would be busyBodyMutations.
		if callbacks > 20 {
			t.Fatalf("the watcher ran %.0f callbacks for a document whose body changed %d times", callbacks, busyBodyMutations)
		}
		t.Logf("the watcher ran %.0f callbacks over %d body mutations", callbacks, busyBodyMutations)
	})
	t.Run("a title inserted into the body after load reads right", func(t *testing.T) {
		p, _ := open(t, "/late")
		ctx, cancel := operationContext(t.Context(), p.Lifetime(), operationTimeout)
		defer cancel()
		pageValue[bool](t, ctx, "window.inserted === true")
		if _, title, err := p.Info(ctx); err != nil || title != "Late" {
			t.Fatalf("an info read answers %q (%v), want Late", title, err)
		}
	})
	t.Run("head insertions in an untitled document stay bounded", func(t *testing.T) {
		p, _ := open(t, "/untitled-head")
		ctx, cancel := operationContext(t.Context(), p.Lifetime(), operationTimeout)
		defer cancel()
		elapsed := pageValue[float64](t, ctx, "window.elapsed")
		callbacks := titleWatchCallbacks(t, p)
		if callbacks > headChildInsertions+20 {
			t.Fatalf("the watcher ran %.0f callbacks for %d head insertions", callbacks, headChildInsertions)
		}
		if elapsed > 2000 {
			t.Fatalf("%d head insertions over a %d-element body took %.0f ms", headChildInsertions, bodyElements, elapsed)
		}
		t.Logf("%d head insertions over a %d-element body: %.0f callbacks, %.1f ms", headChildInsertions, bodyElements, callbacks, elapsed)
	})
}

// pageValue waits for expression to be truthy in the page's own world and
// answers it.
func pageValue[T any](t *testing.T, ctx context.Context, expression string) T {
	t.Helper()
	var zero T
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		value, err := chromedp.Run(ctx, chromedp.Evaluate[T](expression))
		if err == nil && any(value) != any(zero) {
			return value
		}
	}
	t.Fatalf("the page never answered %s", expression)
	return zero
}

// titleWatchCallbacks reads the watcher's callback count from its own
// isolated world: Page.createIsolatedWorld answers the frame's existing
// world of that name.
func titleWatchCallbacks(t *testing.T, p *cdpPage) float64 {
	t.Helper()
	ctx, cancel := operationContext(t.Context(), p.Lifetime(), operationTimeout)
	defer cancel()
	tree, err := chromedp.Call(ctx, page.GetFrameTree, cdp.Empty{})
	if err != nil || tree.FrameTree == nil || tree.FrameTree.Frame == nil {
		t.Fatalf("read the frame tree: %v", err)
	}
	world, err := chromedp.Call(ctx, page.CreateIsolatedWorld, page.CreateIsolatedWorldParams{
		FrameID: tree.FrameTree.Frame.ID, WorldName: documentTitleWorld,
	})
	if err != nil {
		t.Fatalf("reach the watcher's world: %v", err)
	}
	result, err := chromedp.Call(ctx, cdpruntime.Evaluate, cdpruntime.EvaluateParams{
		Expression: `globalThis.__aoDocumentTitleWatch?.callbacks ?? -1`, ContextID: world.ExecutionContextID, ReturnByValue: new(true),
	})
	if err != nil || result.Result == nil {
		t.Fatalf("read the callback count: %v", err)
	}
	count, err := strconv.ParseFloat(string(result.Result.Value), 64)
	if err != nil || count < 0 {
		t.Fatalf("the watcher's world has no callback count (%s): %v", result.Result.Value, err)
	}
	return count
}

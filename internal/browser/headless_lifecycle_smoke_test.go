//go:build !windows

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/procutil/procutiltest"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
)

// TestHeadlessChromiumRealLoadingAndFrames has real headless Chromium
// load a page whose body streams, stop a load by window.stop(), and add
// and remove an iframe and a cross-site one that downloads, and checks
// what the page's ordered input made of it: loading from each start to
// its stop, idle after a stopped load, and each iframe, its id read from
// its element, owned from its attach to its removal, the cross-site one
// across its move to a renderer of its own and when its download begins.
func TestHeadlessChromiumRealLoadingAndFrames(t *testing.T) {
	if os.Getenv("AO_HEADLESS_CHROMIUM_SMOKE") != "1" {
		t.Skip("set AO_HEADLESS_CHROMIUM_SMOKE=1 to launch this machine's real Chromium")
	}
	t.Setenv("HOME", t.TempDir())
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stream":
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, `<!doctype html><title>stream</title><p>start</p>`)
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
			_, _ = fmt.Fprint(w, `<p>end</p>`)
		case "/frame":
			_, _ = fmt.Fprint(w, `<!doctype html><p>frame</p>`)
		case "/frame-download":
			_, _ = fmt.Fprint(w, `<!doctype html><p>frame</p><script>
				const a = document.createElement('a'); a.href = '/file'; a.download = 'f.bin'; document.body.appendChild(a); a.click();
			</script>`)
		case "/file":
			w.Header().Set("Content-Disposition", "attachment; filename=f.bin")
			_, _ = fmt.Fprint(w, "0123456789")
		default:
			_, _ = fmt.Fprint(w, `<!doctype html><title>home</title><p>home</p>`)
		}
	}))
	t.Cleanup(server.Close)
	// The server listens on 127.0.0.1; the same server named localhost is
	// another site, whose iframe Chromium runs in a renderer of its own.
	crossSite := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	// Each download's start, and whether the page owned its frame as the
	// Manager is asked to admit it.
	type begun struct {
		frame string
		owned bool
	}
	downloads := make(chan begun, 4)
	var owner atomic.Pointer[pageDriver]
	engine, err := newHeadlessChromiumEngine(t.TempDir(), HeadlessChromiumOptions{Reaper: procutiltest.Reaper()}, engineEvents{
		PopupOpened:     func(enginePopup) {},
		PageClosed:      func(ids.PageHandle) {},
		PageCrashed:     func(ids.PageHandle, string) {},
		PageInfoChanged: func(ids.PageHandle, string, string, uint64) {},
		DownloadStarted: func(start downloadStart) downloadAdmission {
			owned := false
			if d := owner.Load(); d != nil {
				owned = (*d).OwnsFrame(start.Frame)
			}
			downloads <- begun{frame: string(start.Frame), owned: owned}
			return downloadRefused
		},
		DownloadProgress: func(downloadProgress) {},
	})
	if err != nil {
		t.Fatalf("find a Chromium: %v", err)
	}
	if err := engine.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	profile := testHeadlessProfile(t, engine, t.TempDir(), false)
	defer func() {
		if err := profile.Dispose(context.Background()); err != nil {
			t.Errorf("dispose: %v", err)
		}
	}()
	var mu sync.Mutex
	var kept uint64
	loading, reported := false, false
	hooks := testPageHooks()
	hooks.Loading = func(now bool, order uint64) {
		mu.Lock()
		defer mu.Unlock()
		if order > kept {
			kept, loading, reported = order, now, true
		}
	}
	state := func() (bool, bool) {
		mu.Lock()
		defer mu.Unlock()
		return loading, reported
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	driver, err := profile.NewPage(ctx, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	owner.Store(&driver)
	p := driver.(*cdpPage)
	await := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	run := func(expression string) {
		t.Helper()
		if _, err := chromedp.Run(p.ctx, chromedp.Evaluate[bool](expression+"; true")); err != nil {
			t.Fatalf("%s: %v", expression, err)
		}
	}

	opCtx, opCancel := operationContext(ctx, driver.Lifetime(), operationTimeout)
	defer opCancel()
	if err := driver.Navigate(opCtx, server.URL+"/"); err != nil {
		t.Fatal(err)
	}
	await("the home page to stop loading", func() bool { l, r := state(); return r && !l })
	run(fmt.Sprintf(`location.href = %q`, server.URL+"/stream"))
	await("the streaming page to load", func() bool { l, _ := state(); return l })
	time.Sleep(200 * time.Millisecond)
	if l, _ := state(); !l {
		t.Fatal("the page reads idle while its body streams")
	}
	run(`window.stop()`)
	await("the stopped load to read idle", func() bool { l, _ := state(); return !l })

	// Each iframe's frame id is read from its element in the DOM, an
	// observation apart from the page's frame events and frame tree, which
	// a read of the tree here would feed.
	ownerFrame := func(id string) cdp.FrameID {
		t.Helper()
		var frame cdp.FrameID
		await("iframe #"+id+" to have a frame", func() bool {
			doc, err := chromedp.Call(p.ctx, dom.GetDocument, dom.GetDocumentParams{})
			if err != nil || doc.Root == nil {
				return false
			}
			found, err := chromedp.Call(p.ctx, dom.QuerySelector, dom.QuerySelectorParams{NodeID: doc.Root.NodeID, Selector: "#" + id})
			if err != nil || found.NodeID == 0 {
				return false
			}
			node, err := chromedp.Call(p.ctx, dom.DescribeNode, dom.DescribeNodeParams{NodeID: found.NodeID})
			if err != nil || node.Node == nil {
				return false
			}
			frame = node.Node.FrameID
			return frame != ""
		})
		return frame
	}
	run(`(() => { const f = document.createElement('iframe'); f.id = 'f'; f.src = '/frame'; document.body.appendChild(f); })()`)
	frame := ownerFrame("f")
	await("the page to own its iframe", func() bool { return driver.OwnsFrame(frameID(frame)) })
	run(`document.getElementById('f').remove()`)
	await("the removed iframe to leave the page", func() bool { return !driver.OwnsFrame(frameID(frame)) })

	// A cross-site iframe runs in a renderer of its own, and a download it
	// starts names its frame, which the page still owns.
	run(fmt.Sprintf(`(() => { const f = document.createElement('iframe'); f.id = 'x'; f.src = %q; document.body.appendChild(f); })()`, crossSite+"/frame-download"))
	cross := ownerFrame("x")
	select {
	case start := <-downloads:
		if start.frame != string(cross) {
			t.Fatalf("the cross-site iframe's download began in frame %s, its element's frame is %s", start.frame, cross)
		}
		if !start.owned {
			t.Fatal("the page did not own the cross-site iframe its download began in")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cross-site iframe's download never began")
	}
	if !driver.OwnsFrame(frameID(cross)) {
		t.Fatal("the page let its cross-site iframe go while it is still there")
	}
	run(`document.getElementById('x').remove()`)
	await("the removed cross-site iframe to leave the page", func() bool { return !driver.OwnsFrame(frameID(cross)) })
	releaseOnce.Do(func() { close(release) })
}

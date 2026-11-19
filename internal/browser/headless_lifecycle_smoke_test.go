//go:build !windows

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// TestHeadlessChromiumRealLoadingAndFrames has real headless Chromium
// load a page whose body streams, stop a load by window.stop(), and add
// and remove an iframe, and checks what the page's ordered input made of
// it: loading from each start to its stop, idle after a stopped load, and
// the iframe owned from its attach to its detach.
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
		default:
			_, _ = fmt.Fprint(w, `<!doctype html><title>home</title><p>home</p>`)
		}
	}))
	t.Cleanup(server.Close)
	engine, err := newHeadlessChromiumEngine(t.TempDir(), HeadlessChromiumOptions{Reaper: procutiltest.Reaper()}, engineEvents{
		PopupOpened:      func(enginePopup) {},
		PageClosed:       func(string) {},
		PageCrashed:      func(string, string) {},
		PageInfoChanged:  func(string, string, string, uint64) {},
		DownloadStarted:  func(downloadStart) downloadAdmission { return downloadAdmitted },
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

	run(`(() => { const f = document.createElement('iframe'); f.id = 'f'; f.src = '/frame'; document.body.appendChild(f); })()`)
	var frame cdp.FrameID
	await("the iframe to be in the frame tree", func() bool {
		tree, err := chromedp.Call(p.ctx, page.GetFrameTree, cdp.Empty{})
		if err != nil || tree.FrameTree == nil || len(tree.FrameTree.ChildFrames) == 0 {
			return false
		}
		frame = tree.FrameTree.ChildFrames[0].Frame.ID
		return true
	})
	await("the page to own its iframe", func() bool { return driver.OwnsFrame(string(frame)) })
	run(`document.getElementById('f').remove()`)
	await("the removed iframe to leave the page", func() bool { return !driver.OwnsFrame(string(frame)) })
	releaseOnce.Do(func() { close(release) })
}

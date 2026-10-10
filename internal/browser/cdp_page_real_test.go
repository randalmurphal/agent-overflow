//go:build !windows

package browser

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// realOperationsPage exercises every page operation the CDP driver sends:
// a button and an input for clicks and keys, a press and a release target
// for a pointer drag, a child frame, a download link, a console line and a
// tall body to scroll.
const realOperationsPage = `<!doctype html><html><head><title>operations</title></head>
<body style="height:3000px;margin:0">
<input id="name" value="old">
<button id="btn" onclick="document.body.dataset.clicked=String((+document.body.dataset.clicked||0)+1)">Go</button>
<div id="drag" style="width:60px;height:60px;background:red" onmousedown="document.body.dataset.pressed=this.id"></div>
<div id="drop" style="width:60px;height:60px;background:blue" onmouseup="document.body.dataset.released=this.id"></div>
<iframe id="child" srcdoc="<p>inner</p>"></iframe>
<a id="dl" href="data:text/plain,downloaded-bytes" download="hello.txt">download</a>
<script>
console.log("ready", 1);
addEventListener("keydown", e => { if (e.key === "x" && e.ctrlKey) document.body.dataset.chord = "ctrl-x" });
</script>
</body></html>`

// assertRealPageOperations runs the CDP driver's page operations against a
// real Chromium: input, pointer drags, scrolling, screenshots, history,
// network idle and the frame set (through the page's event subscriptions),
// locators, snapshots, assets, console capture and downloads (through the
// browser-level download subscriptions).
func assertRealPageOperations(t *testing.T, engine *headlessEngine) {
	t.Helper()
	workspace := t.TempDir()
	index := filepath.Join(workspace, "index.html")
	second := filepath.Join(workspace, "second.html")
	asset := filepath.Join(workspace, "asset.txt")
	for path, body := range map[string]string{index: realOperationsPage, second: "<title>second</title>second", asset: "asset-bytes"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var consoleLines []string
	var downloads []downloadProgress
	engine.events.DownloadStarted = func(downloadStart) bool { return true }
	engine.events.DownloadProgress = func(progress downloadProgress) {
		mu.Lock()
		downloads = append(downloads, progress)
		mu.Unlock()
	}
	profile := testHeadlessProfile(t, engine, workspace, false)
	defer func() {
		if err := profile.Dispose(t.Context()); err != nil {
			t.Errorf("dispose: %v", err)
		}
	}()
	hooks := testPageHooks()
	hooks.Console = func(entry ConsoleLog) {
		mu.Lock()
		consoleLines = append(consoleLines, entry.Level+" "+entry.Message)
		mu.Unlock()
	}
	driver, err := profile.NewPage(t.Context(), hooks)
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	p := driver.(*cdpPage)
	ctx, cancel := operationContext(t.Context(), p.Lifetime(), operationTimeout)
	defer cancel()
	if err := p.Navigate(ctx, "file://"+index); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := p.SetViewport(ctx, 800, 600); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	eventually := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(headlessTestDeadline)
		for !done() {
			if time.Now().After(deadline) {
				t.Fatalf("never saw %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	read := func(expression string) string {
		t.Helper()
		value, err := chromedp.Run(ctx, chromedp.Evaluate[string](expression))
		if err != nil {
			t.Fatalf("read %s: %v", expression, err)
		}
		return value
	}
	center := func(selector string) Point {
		t.Helper()
		point, err := chromedp.Run(ctx, chromedp.Evaluate[Point](`(() => { const r = document.querySelector(`+jsonString(selector)+`).getBoundingClientRect(); return {x: r.x + r.width / 2, y: r.y + r.height / 2}; })()`))
		if err != nil {
			t.Fatalf("locate %s: %v", selector, err)
		}
		return point
	}

	eventually("network idle after the load", func() bool {
		status, err := p.PageStatus(ctx)
		return err == nil && status.Ready == "complete" && status.NetworkIdle
	})
	eventually("the console line", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(consoleLines) > 0 && consoleLines[0] == "log ready 1"
	})

	if err := p.Click(ctx, "#btn"); err != nil {
		t.Fatalf("click: %v", err)
	}
	if got := read(`document.body.dataset.clicked || ""`); got != "1" {
		t.Fatalf("after Click the button saw %q clicks, want 1", got)
	}
	if err := p.Type(ctx, "#name", "new", true); err != nil {
		t.Fatalf("type: %v", err)
	}
	if err := p.TypeText(ctx, "abc"); err != nil {
		t.Fatalf("type text: %v", err)
	}
	if got := read(`document.querySelector("#name").value`); got != "newabc" {
		t.Fatalf("after Type with clear and TypeText the input holds %q, want newabc", got)
	}
	if err := p.Press(ctx, "Control+x"); err != nil {
		t.Fatalf("press: %v", err)
	}
	if got := read(`document.body.dataset.chord || ""`); got != "ctrl-x" {
		t.Fatalf("after Press Control+x the page saw %q", got)
	}
	button := center("#btn")
	if err := p.Pointer(ctx, PointerOptions{Action: "click", X: button.X, Y: button.Y}); err != nil {
		t.Fatalf("pointer click: %v", err)
	}
	if got := read(`document.body.dataset.clicked || ""`); got != "2" {
		t.Fatalf("after a pointer click the button saw %q clicks, want 2", got)
	}
	from, to := center("#drag"), center("#drop")
	if err := p.Pointer(ctx, PointerOptions{Action: "drag", Path: []Point{from, {X: (from.X + to.X) / 2, Y: (from.Y + to.Y) / 2}, to}}); err != nil {
		t.Fatalf("pointer drag: %v", err)
	}
	if got := read(`(document.body.dataset.pressed || "") + ">" + (document.body.dataset.released || "")`); got != "drag>drop" {
		t.Fatalf("the drag pressed and released on %q, want drag>drop", got)
	}

	matches, err := p.ResolveLocator(ctx, Locator{CSS: "#btn"}, "")
	if err != nil || len(matches) != 1 {
		t.Fatalf("resolve the button: %v, %d matches", err, len(matches))
	}
	if text, err := p.ReadNode(ctx, matches[0], Locator{CSS: "#btn"}, "innerText", ""); err != nil || text != "Go" {
		t.Fatalf("read the button: %v, %v", text, err)
	}
	if err := p.ActOnNode(ctx, matches[0], Locator{CSS: "#btn"}, nodeAction{Kind: "click", Clicks: 1}); err != nil {
		t.Fatalf("click the located button: %v", err)
	}
	if got := read(`document.body.dataset.clicked || ""`); got != "3" {
		t.Fatalf("after a located click the button saw %q clicks, want 3", got)
	}
	snapshot, err := p.Snapshot(ctx)
	if err != nil || snapshot.Title != "operations" || len(snapshot.Elements) == 0 {
		t.Fatalf("snapshot: %v, %+v", err, snapshot.PageInfo)
	}

	tree, err := chromedp.Call(ctx, page.GetFrameTree, cdp.Empty{})
	if err != nil || tree.FrameTree == nil || len(tree.FrameTree.ChildFrames) != 1 {
		t.Fatalf("read the frame tree: %v", err)
	}
	child := tree.FrameTree.ChildFrames[0].Frame.ID
	eventually("the child frame owned", func() bool { return p.OwnsFrame(string(child)) })
	if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`document.querySelector("#child").remove(), true`)); err != nil {
		t.Fatalf("remove the child frame: %v", err)
	}
	eventually("the removed child frame released", func() bool { return !p.OwnsFrame(string(child)) })

	if err := p.Scroll(ctx, "", 0, 500); err != nil {
		t.Fatalf("scroll: %v", err)
	}
	for _, opts := range []ScreenshotOptions{{}, {FullPage: true}, {Clip: &ClipRect{X: 0, Y: 0, Width: 100, Height: 100}}} {
		image, err := p.Screenshot(ctx, opts)
		if err != nil {
			t.Fatalf("screenshot %+v: %v", opts, err)
		}
		if !bytes.HasPrefix(image, []byte{0xff, 0xd8}) {
			t.Fatalf("screenshot %+v is not a JPEG", opts)
		}
		if got := read(`String(window.scrollY)`); got != "500" {
			t.Fatalf("screenshot %+v left the page scrolled to %s, want 500", opts, got)
		}
	}

	fetch, err := p.AssetFetcher(ctx)
	if err != nil {
		t.Fatalf("asset fetcher: %v", err)
	}
	// Chromium loads no file:// URL as a network resource, so the asset is
	// served from loopback.
	server := httptest.NewServer(http.FileServer(http.Dir(workspace)))
	defer server.Close()
	stream, err := fetch(server.URL + "/asset.txt")
	if err != nil {
		t.Fatalf("fetch an asset: %v", err)
	}
	var body bytes.Buffer
	_, err = stream.Copy(&body, 1<<20, 1<<20)
	stream.Close()
	if err != nil || body.String() != "asset-bytes" {
		t.Fatalf("read the asset: %q, %v", body.String(), err)
	}
	if _, err := p.AssetInventory(ctx); err != nil {
		t.Fatalf("asset inventory: %v", err)
	}

	if err := p.Click(ctx, "#dl"); err != nil {
		t.Fatalf("click the download link: %v", err)
	}
	eventually("the download completed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, progress := range downloads {
			if progress.State == downloadCompleted && progress.Received == float64(len("downloaded-bytes")) {
				return true
			}
		}
		return false
	})

	if err := p.Navigate(ctx, "file://"+second); err != nil {
		t.Fatalf("navigate to the second page: %v", err)
	}
	if err := p.History(ctx, "back"); err != nil {
		t.Fatalf("history back: %v", err)
	}
	eventually("the first page after back", func() bool {
		location, _, err := p.Info(ctx)
		return err == nil && strings.HasSuffix(location, "/index.html")
	})
	// The page opened at about:blank, so there is history behind the first
	// page as well as the second page ahead of it.
	if back, forward, err := p.HistoryState(ctx); err != nil || !back || !forward {
		t.Fatalf("history state after back: back=%v forward=%v err=%v", back, forward, err)
	}
	if err := p.History(ctx, "forward"); err != nil {
		t.Fatalf("history forward: %v", err)
	}
	eventually("the second page after forward", func() bool {
		_, title, err := p.Info(ctx)
		return err == nil && title == "second"
	})
	if err := p.History(ctx, "reload"); err != nil {
		t.Fatalf("history reload: %v", err)
	}
	eventually("network idle after the reload", func() bool {
		status, err := p.PageStatus(ctx)
		return err == nil && status.Ready == "complete" && status.NetworkIdle
	})
	p.Close()
}

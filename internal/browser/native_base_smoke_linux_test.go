//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// TestNativeEngineRealPageBase shows a page whose document has no
// background of its own and blank tabs in one pane, switching between them
// in both orders and navigating the shown tab between the two, and reads the
// pane from the compositor's screenshot of the output: white under the
// document, the pane's color under a blank tab (pageBase).
//
// A view that never loaded paints nothing at all (the SPA's pane surface
// shows through it) and WebKit cannot snapshot it, so each blank tab first
// navigates itself to about:blank, which the Manager hears only from the
// engine's report.
//
// AO_NATIVE_BROWSER_SMOKE_SHOTS names a directory that keeps each screenshot.
func TestNativeEngineRealPageBase(t *testing.T) {
	window := smokeWindow(t)
	if os.Getenv(nativeSmokeWestonEnv) == "" {
		t.Skip("the screenshot needs the run's own weston")
	}
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	workspace := resolvedTempDir(t)
	access := manager.Bind(Caller{ThreadID: "native-smoke-base", Workspace: workspace})
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID()); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	file := writeSmokeFile(t, workspace, "plain.html", `<!doctype html><title>plain</title>`)
	doc, err := manager.OpenFile(ctx, access, file, OpenOptions{})
	if err != nil {
		t.Fatalf("open the document: %v", err)
	}
	blank, err := manager.NewCompanionPage(ctx, access, "")
	if err != nil {
		t.Fatalf("open a blank tab: %v", err)
	}
	blank2, err := manager.NewCompanionPage(ctx, access, "")
	if err != nil {
		t.Fatalf("open a second blank tab: %v", err)
	}
	show := true
	if _, err := manager.Visibility(ctx, access, &show, PageID(blank.ID)); err != nil {
		t.Fatalf("show the pane: %v", err)
	}
	mount, err := manager.AttachPane(access.Caller(), untiedPane)
	if err != nil {
		t.Fatalf("mount a pane: %v", err)
	}
	t.Cleanup(func() { manager.DetachPane(MountID(mount.ID)) })
	pane := color.RGBA{0x1e, 0x1e, 0x2e, 0xff}
	white := color.RGBA{0xff, 0xff, 0xff, 0xff}
	if err := manager.SetPaneRect(MountID(mount.ID), PaneRect{
		X: 200, Y: 100, Width: 640, Height: 400, ClipX: 200, ClipY: 100, ClipWidth: 640, ClipHeight: 400,
		ViewportWidth: 1280, ViewportHeight: 800, Visible: true, Background: "#1e1e2e",
	}); err != nil {
		t.Fatalf("place the pane: %v", err)
	}
	shots := os.Getenv("AO_NATIVE_BROWSER_SMOKE_SHOTS")
	var region image.Rectangle
	// painted waits for the pane region to be want: a base change reaches the
	// view on the GTK thread, and the compositor, after the call that asked.
	painted := func(step string, want color.RGBA) {
		t.Helper()
		var share float64
		var last []byte
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			img, raw := westonScreenshot(t)
			last = raw
			if region.Empty() {
				// The blank tab shows first: the pane is where its color is.
				region = boundsOf(img, pane)
				if region.Dx() < 100 || region.Dy() < 100 {
					continue
				}
			}
			share = shareOf(img, region, want)
			if share > 0.99 {
				if shots != "" {
					if err := os.WriteFile(filepath.Join(shots, step+".png"), raw, 0o644); err != nil {
						t.Errorf("%s: keep the screenshot: %v", step, err)
					}
				}
				return
			}
		}
		if shots != "" && last != nil {
			if err := os.WriteFile(filepath.Join(shots, step+"-failed.png"), last, 0o644); err != nil {
				t.Errorf("%s: keep the screenshot: %v", step, err)
			}
		}
		t.Fatalf("%s: %.0f%% of the pane region %v is %v", step, share*100, region, want)
	}
	activate := func(pageID string) {
		t.Helper()
		if err := manager.ActivateCompanionPage(ctx, access, PageID(pageID)); err != nil {
			t.Fatalf("activate %s: %v", pageID, err)
		}
	}
	// navigateItself moves the page as a script would, so only the engine's
	// report tells the Manager.
	navigateItself := func(pageID, address string) {
		t.Helper()
		p, _, err := manager.lookupOwnedPage(access, PageID(pageID))
		if err != nil {
			t.Fatal(err)
		}
		if err := p.driver.Navigate(ctx, address); err != nil {
			t.Fatalf("navigate %s to %s: %v", pageID, address, err)
		}
	}
	activate(blank.ID)
	navigateItself(blank.ID, "about:blank")
	painted("1-blank-first", pane)
	activate(doc.ID)
	painted("2-document-after-blank", white)
	navigateItself(blank2.ID, "about:blank")
	activate(blank2.ID)
	painted("3-blank-after-document", pane)
	if _, err := manager.NavigateCompanion(ctx, access, PageID(blank2.ID), smokeFileURL(file)); err != nil {
		t.Fatalf("navigate the blank tab: %v", err)
	}
	painted("4-blank-tab-navigated-to-the-document", white)
	navigateItself(blank2.ID, "about:blank")
	painted("5-document-navigated-itself-to-blank", pane)
}

// TestNativeEngineRealHiddenPageBase presents a blank tab on the pane's
// color, hides the pane, navigates the hidden tab to a document with no
// background of its own and reads it with Manager.Screenshot: white, as in
// any browser. A base that only a presentation sets keeps the pane's color
// under the hidden document. The shown blank tab's own screenshot is read
// first, so the snapshot is known to include the base.
func TestNativeEngineRealHiddenPageBase(t *testing.T) {
	window := smokeWindow(t)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	workspace := resolvedTempDir(t)
	access := manager.Bind(Caller{ThreadID: "native-smoke-hidden-base", Workspace: workspace})
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID()); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	file := writeSmokeFile(t, workspace, "plain.html", `<!doctype html><title>plain</title>`)
	blank, err := manager.NewCompanionPage(ctx, access, "")
	if err != nil {
		t.Fatalf("open a blank tab: %v", err)
	}
	p, _, err := manager.lookupOwnedPage(access, PageID(blank.ID))
	if err != nil {
		t.Fatal(err)
	}
	// A view that never loaded cannot be snapshotted.
	if err := p.driver.Navigate(ctx, "about:blank"); err != nil {
		t.Fatalf("load about:blank: %v", err)
	}
	show := true
	if _, err := manager.Visibility(ctx, access, &show, PageID(blank.ID)); err != nil {
		t.Fatalf("show the pane: %v", err)
	}
	mount, err := manager.AttachPane(access.Caller(), untiedPane)
	if err != nil {
		t.Fatalf("mount a pane: %v", err)
	}
	t.Cleanup(func() { manager.DetachPane(MountID(mount.ID)) })
	if err := manager.SetPaneRect(MountID(mount.ID), PaneRect{
		X: 200, Y: 100, Width: 640, Height: 400, ClipX: 200, ClipY: 100, ClipWidth: 640, ClipHeight: 400,
		ViewportWidth: 1280, ViewportHeight: 800, Visible: true, Background: "#1e1e2e",
	}); err != nil {
		t.Fatalf("place the pane: %v", err)
	}
	shot := func(step string, want color.RGBA) {
		t.Helper()
		var share float64
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			raw, err := manager.Screenshot(ctx, access, ScreenshotOptions{PageID: PageID(blank.ID)})
			if err != nil {
				t.Fatalf("%s: screenshot: %v", step, err)
			}
			img, err := jpeg.Decode(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("%s: decode the screenshot: %v", step, err)
			}
			if share = shareNear(img, want); share > 0.99 {
				return
			}
		}
		t.Fatalf("%s: %.0f%% of the screenshot is %v", step, share*100, want)
	}
	shot("shown blank tab", color.RGBA{0x1e, 0x1e, 0x2e, 0xff})
	hide := false
	if _, err := manager.Visibility(ctx, access, &hide, ""); err != nil {
		t.Fatalf("hide the pane: %v", err)
	}
	if _, err := manager.NavigateCompanion(ctx, access, PageID(blank.ID), smokeFileURL(file)); err != nil {
		t.Fatalf("navigate the hidden tab: %v", err)
	}
	shot("hidden document", color.RGBA{0xff, 0xff, 0xff, 0xff})
}

// heldBaseEngine is the real WebKitGTK engine with its first white base
// held until released, as a GTK thread busy elsewhere holds it.
type heldBaseEngine struct {
	*webkitEngine
	handle  ids.PageHandle
	entered chan struct{}
	gate    chan struct{}
	held    atomic.Bool
}

var _ paneHost = (*heldBaseEngine)(nil)

func (e *heldBaseEngine) SetPageBase(handle ids.PageHandle, color string) {
	if handle == e.handle && color == documentBase && e.held.CompareAndSwap(false, true) {
		e.entered <- struct{}{}
		<-e.gate
	}
	e.webkitEngine.SetPageBase(handle, color)
}

// TestNativeEngineRealHeldBase shows a blank tab on the pane's color, hides
// it, navigates it to a document with no background of its own while the
// white base that address calls for is held, and shows it: the pane does not
// show the page on the pane's color while the base is held, and shows it
// white once it lands.
func TestNativeEngineRealHeldBase(t *testing.T) {
	window := smokeWindow(t)
	if os.Getenv(nativeSmokeWestonEnv) == "" {
		t.Skip("the screenshot needs the run's own weston")
	}
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	engine := &heldBaseEngine{webkitEngine: manager.engine.(*webkitEngine), entered: make(chan struct{}, 1), gate: make(chan struct{})}
	manager.engine = engine
	release := sync.OnceFunc(func() { close(engine.gate) })
	t.Cleanup(func() {
		release()
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	workspace := resolvedTempDir(t)
	access := manager.Bind(Caller{ThreadID: "native-smoke-held-base", Workspace: workspace})
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID()); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	file := writeSmokeFile(t, workspace, "plain.html", `<!doctype html><title>plain</title>`)
	blank, err := manager.NewCompanionPage(ctx, access, "")
	if err != nil {
		t.Fatalf("open a blank tab: %v", err)
	}
	p, _, err := manager.lookupOwnedPage(access, PageID(blank.ID))
	if err != nil {
		t.Fatal(err)
	}
	engine.handle = p.driver.Handle()
	if err := p.driver.Navigate(ctx, "about:blank"); err != nil {
		t.Fatalf("load about:blank: %v", err)
	}
	show := true
	if _, err := manager.Visibility(ctx, access, &show, PageID(blank.ID)); err != nil {
		t.Fatalf("show the pane: %v", err)
	}
	mount, err := manager.AttachPane(access.Caller(), untiedPane)
	if err != nil {
		t.Fatalf("mount a pane: %v", err)
	}
	t.Cleanup(func() { manager.DetachPane(MountID(mount.ID)) })
	if err := manager.SetPaneRect(MountID(mount.ID), PaneRect{
		X: 200, Y: 100, Width: 640, Height: 400, ClipX: 200, ClipY: 100, ClipWidth: 640, ClipHeight: 400,
		ViewportWidth: 1280, ViewportHeight: 800, Visible: true, Background: "#1e1e2e",
	}); err != nil {
		t.Fatalf("place the pane: %v", err)
	}
	pane := color.RGBA{0x1e, 0x1e, 0x2e, 0xff}
	white := color.RGBA{0xff, 0xff, 0xff, 0xff}
	var region image.Rectangle
	share := func(want color.RGBA) float64 {
		img, _ := westonScreenshot(t)
		if region.Empty() {
			region = boundsOf(img, pane)
		}
		return shareOf(img, region, want)
	}
	waitFor := func(step string, want color.RGBA) {
		t.Helper()
		var got float64
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if got = share(want); got > 0.99 && region.Dx() >= 100 && region.Dy() >= 100 {
				return
			}
		}
		t.Fatalf("%s: %.0f%% of the pane region %v is %v", step, got*100, region, want)
	}
	waitFor("blank tab shown", pane)
	hide := false
	if _, err := manager.Visibility(ctx, access, &hide, ""); err != nil {
		t.Fatalf("hide the pane: %v", err)
	}
	if err := p.driver.Navigate(ctx, smokeFileURL(file)); err != nil {
		t.Fatalf("navigate the hidden tab: %v", err)
	}
	select {
	case <-engine.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the white base was never applied")
	}
	shown := make(chan error, 1)
	go func() {
		_, err := manager.Visibility(ctx, access, &show, PageID(blank.ID))
		shown <- err
	}()
	time.Sleep(time.Second)
	if got := share(pane); got > 0.99 {
		t.Fatalf("the document was shown on the pane's color while its white base was held (%.0f%% of %v)", got*100, region)
	}
	release()
	if err := <-shown; err != nil {
		t.Fatalf("show the pane: %v", err)
	}
	waitFor("document shown on its base", white)
}

// TestNativeEngineRealStaticPageShowsAgain hides a static page (one solid
// color, no script, no animation) every way the pane hides a page and shows
// it again, and reads the pane from the compositor's screenshot each time:
// the page is on screen again without drawing a new frame of its own. A
// view moved to another widget parent loses the frame WebKit last drew, and
// a static page never draws another, so this fails while hiding reparents.
//
// The screenshot is live, not a stale buffer: the hidden state is checked
// in the same region first, and the pink page shown in between is.
func TestNativeEngineRealStaticPageShowsAgain(t *testing.T) {
	window := smokeWindow(t)
	if os.Getenv(nativeSmokeWestonEnv) == "" {
		t.Skip("the screenshot needs the run's own weston")
	}
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	workspace := resolvedTempDir(t)
	access := manager.Bind(Caller{ThreadID: "native-smoke-show-again", Workspace: workspace})
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID()); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	blue := color.RGBA{0x2a, 0x7f, 0xd0, 0xff}
	pink := color.RGBA{0xc0, 0x40, 0x80, 0xff}
	page, err := manager.OpenFile(ctx, access, writeSmokeFile(t, workspace, "blue.html",
		`<!doctype html><title>blue</title><style>html{background:#2a7fd0}</style>`), OpenOptions{})
	if err != nil {
		t.Fatalf("open the static page: %v", err)
	}
	other, err := manager.OpenFile(ctx, access, writeSmokeFile(t, workspace, "pink.html",
		`<!doctype html><title>pink</title><style>html{background:#c04080}</style>`), OpenOptions{})
	if err != nil {
		t.Fatalf("open the other page: %v", err)
	}
	show := true
	if _, err := manager.Visibility(ctx, access, &show, PageID(page.ID)); err != nil {
		t.Fatalf("show the pane: %v", err)
	}
	activate := func(pageID string) {
		t.Helper()
		if err := manager.ActivateCompanionPage(ctx, access, PageID(pageID)); err != nil {
			t.Fatalf("activate %s: %v", pageID, err)
		}
	}
	activate(page.ID)
	rect := PaneRect{
		X: 200, Y: 100, Width: 640, Height: 400, ClipX: 200, ClipY: 100, ClipWidth: 640, ClipHeight: 400,
		ViewportWidth: 1280, ViewportHeight: 800, Visible: true, Background: "#1e1e2e",
	}
	mount, err := manager.AttachPane(access.Caller(), untiedPane)
	if err != nil {
		t.Fatalf("mount a pane: %v", err)
	}
	place := func(r PaneRect) {
		t.Helper()
		if err := manager.SetPaneRect(MountID(mount.ID), r); err != nil {
			t.Fatalf("place the pane: %v", err)
		}
	}
	place(rect)
	shots := os.Getenv("AO_NATIVE_BROWSER_SMOKE_SHOTS")
	var region image.Rectangle
	// shows waits up to 3s for the pane region to be mostly want (not)
	// and keeps the last screenshot.
	shows := func(step string, want color.RGBA, yes bool) {
		t.Helper()
		var share float64
		var last []byte
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			img, raw := westonScreenshot(t)
			last = raw
			if region.Empty() {
				region = boundsOf(img, blue)
				if region.Dx() < 100 || region.Dy() < 100 {
					region = image.Rectangle{}
					continue
				}
			}
			share = shareOf(img, region, want)
			if (share > 0.99) == yes {
				break
			}
		}
		if shots != "" && last != nil {
			if err := os.WriteFile(filepath.Join(shots, step+".png"), last, 0o644); err != nil {
				t.Errorf("%s: keep the screenshot: %v", step, err)
			}
		}
		if (share > 0.99) != yes {
			t.Fatalf("%s: %.0f%% of the pane region %v is %v", step, share*100, region, want)
		}
	}
	shows("0-first-show", blue, true)

	// A page opened while the pane is up is laid out at the pane's size
	// before its first show, so that show changes nothing about its frame.
	green := color.RGBA{0x30, 0xa0, 0x50, 0xff}
	third, err := manager.OpenFile(ctx, access, writeSmokeFile(t, workspace, "green.html",
		`<!doctype html><title>green</title><style>html{background:#30a050}</style>`), OpenOptions{})
	if err != nil {
		t.Fatalf("open a page under the shown pane: %v", err)
	}
	shows("0-opened-under-the-pane", blue, true)
	activate(third.ID)
	shows("0-first-show-at-the-panes-size", green, true)
	activate(page.ID)
	shows("0-back", blue, true)

	activate(other.ID)
	shows("1-switched-away", pink, true)
	activate(page.ID)
	shows("1-switched-back", blue, true)

	manager.DetachPane(MountID(mount.ID))
	shows("2-pane-detached", blue, false)
	if mount, err = manager.AttachPane(access.Caller(), untiedPane); err != nil {
		t.Fatalf("mount the pane again: %v", err)
	}
	place(rect)
	shows("2-pane-attached-again", blue, true)

	covered := rect
	covered.Visible = false
	place(covered)
	shows("3-covered-by-an-overlay", blue, false)
	place(rect)
	shows("3-uncovered", blue, true)

	activate(other.ID)
	shows("4-switched-away", pink, true)
	narrower := rect
	narrower.Width, narrower.ClipWidth = 600, 600
	place(narrower)
	place(rect)
	activate(page.ID)
	shows("4-back-after-a-resize-while-hidden", blue, true)
}

//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
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
	window := nativeSmokeWindow.Load()
	if window == nil {
		t.Skipf("set %s=1 to drive this platform's browser engine in a real window", nativeBrowserSmokeEnv)
	}
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
	access := Access{ThreadID: "native-smoke-base", Workspace: workspace}
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
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
	if _, err := manager.Visibility(ctx, access, &show, blank.ID); err != nil {
		t.Fatalf("show the pane: %v", err)
	}
	mount, err := manager.AttachPane(access)
	if err != nil {
		t.Fatalf("mount a pane: %v", err)
	}
	t.Cleanup(func() { manager.DetachPane(mount.ID) })
	pane := color.RGBA{0x1e, 0x1e, 0x2e, 0xff}
	white := color.RGBA{0xff, 0xff, 0xff, 0xff}
	if err := manager.SetPaneRect(mount.ID, PaneRect{
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
		if err := manager.ActivateCompanionPage(ctx, access, pageID); err != nil {
			t.Fatalf("activate %s: %v", pageID, err)
		}
	}
	// navigateItself moves the page as a script would, so only the engine's
	// report tells the Manager.
	navigateItself := func(pageID, address string) {
		t.Helper()
		p, _, err := manager.lookupOwnedPage(access, pageID)
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
	if _, err := manager.NavigateCompanion(ctx, access, blank2.ID, smokeFileURL(file)); err != nil {
		t.Fatalf("navigate the blank tab: %v", err)
	}
	painted("4-blank-tab-navigated-to-the-document", white)
	navigateItself(blank2.ID, "about:blank")
	painted("5-document-navigated-itself-to-blank", pane)
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
	window := nativeSmokeWindow.Load()
	if window == nil {
		t.Skipf("set %s=1 to drive this platform's browser engine in a real window", nativeBrowserSmokeEnv)
	}
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
	access := Access{ThreadID: "native-smoke-show-again", Workspace: workspace}
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
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
	if _, err := manager.Visibility(ctx, access, &show, page.ID); err != nil {
		t.Fatalf("show the pane: %v", err)
	}
	activate := func(pageID string) {
		t.Helper()
		if err := manager.ActivateCompanionPage(ctx, access, pageID); err != nil {
			t.Fatalf("activate %s: %v", pageID, err)
		}
	}
	activate(page.ID)
	rect := PaneRect{
		X: 200, Y: 100, Width: 640, Height: 400, ClipX: 200, ClipY: 100, ClipWidth: 640, ClipHeight: 400,
		ViewportWidth: 1280, ViewportHeight: 800, Visible: true, Background: "#1e1e2e",
	}
	mount, err := manager.AttachPane(access)
	if err != nil {
		t.Fatalf("mount a pane: %v", err)
	}
	place := func(r PaneRect) {
		t.Helper()
		if err := manager.SetPaneRect(mount.ID, r); err != nil {
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

	manager.DetachPane(mount.ID)
	shows("2-pane-detached", blue, false)
	if mount, err = manager.AttachPane(access); err != nil {
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

// westonScreenshot captures the run's weston output with
// weston-screenshooter, which the compositor allows under --debug.
func westonScreenshot(t *testing.T) (image.Image, []byte) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("weston-screenshooter")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("weston-screenshooter: %v: %s", err, out)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.png"))
	if err != nil || len(files) != 1 {
		t.Fatalf("weston-screenshooter wrote %v (%v)", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode the screenshot: %v", err)
	}
	return img, raw
}

func sameRGB(c color.Color, want color.RGBA) bool {
	r, g, b, _ := c.RGBA()
	return uint8(r>>8) == want.R && uint8(g>>8) == want.G && uint8(b>>8) == want.B
}

// boundsOf answers the smallest rectangle holding every pixel of want.
func boundsOf(img image.Image, want color.RGBA) image.Rectangle {
	var out image.Rectangle
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if sameRGB(img.At(x, y), want) {
				out = out.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	return out
}

// shareOf answers the share of region's pixels that are want.
func shareOf(img image.Image, region image.Rectangle, want color.RGBA) float64 {
	n, hit := 0, 0
	for y := region.Min.Y; y < region.Max.Y; y++ {
		for x := region.Min.X; x < region.Max.X; x++ {
			n++
			if sameRGB(img.At(x, y), want) {
				hit++
			}
		}
	}
	if n == 0 {
		return 0
	}
	return float64(hit) / float64(n)
}

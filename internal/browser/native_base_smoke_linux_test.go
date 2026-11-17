//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
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
// engine's report. Each switch shows a page for the first time: under this
// compositor a page shown again after a hide paints nothing until its
// document draws a new frame.
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
	f, err := os.Open(files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
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

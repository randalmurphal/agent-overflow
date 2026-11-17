//go:build windows

package webview2host

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image/png"
	"os"
	"testing"
	"time"
	"unsafe"
)

// The backend sends a page's base on its own directive, whether or not the
// page has bounds or is shown (internal/browser/page_base.go). This runs it
// on the real runtime and proves that a background sent to a hidden page
// with no bounds yet becomes the controller's default background, that a
// later bounds directive (which carries no color) keeps it, and that the
// hidden page's capture of a document without a background of its own is
// painted on it.
//
//	AO_NATIVE_BROWSER_SMOKE=1 go test -run TestBackgroundSmoke ./internal/webview2host
func TestBackgroundSmokeReachesAHiddenPageWithNoBounds(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	dataDir, err := os.MkdirTemp("", "ao-background-smoke-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := clearUserDataDir(dataDir); err != nil {
			t.Errorf("clear %s: %v", dataDir, err)
		}
		if err := os.Remove(dataDir); err != nil {
			t.Errorf("remove %s: %v", dataDir, err)
		}
	})
	ui := startSmokeUIThread(t)
	reports := make(chan smokeReport, 16)
	host, err := New(Config{
		HostWindow:  func() uintptr { return ui.hwnd },
		UserDataDir: dataDir,
		Logf:        t.Logf,
		Report: func(pageID string, kind ReportKind, detail string) {
			reports <- smokeReport{pageID, kind, detail}
		},
		OnMain: ui.do,
		Post:   ui.post,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		host.Close()
		ui.do(host.env.release)
	})
	host.Apply(Directive{Op: OpCreate, PageID: "page-1", ProfileID: "smoke"})
	var targetID string
	for targetID == "" {
		select {
		case report := <-reports:
			if report.pageID == "page-1" && report.kind == ReportCreated {
				targetID = report.detail
			} else if report.pageID == "page-1" && report.kind == ReportCreateFailed {
				t.Fatalf("create answered %+v", report)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("no created report")
		}
	}
	var page *hostPage
	ui.do(func() { page = host.pages["page-1"] })
	if page == nil || page.controller2 == nil {
		t.Fatal("the runtime has no ICoreWebView2Controller2")
	}
	want := paneColor{A: 255, R: 0x1e, G: 0x1e, B: 0x2e}
	readBackground := func(step string) {
		t.Helper()
		var got paneColor
		var hr uintptr
		ui.do(func() {
			hr, _, _ = page.controller2.vtbl.GetDefaultBackgroundColor.Call(
				uintptr(unsafe.Pointer(page.controller2)), uintptr(unsafe.Pointer(&got)))
		})
		if err := hresult(hr); err != nil {
			t.Fatalf("%s: get_DefaultBackgroundColor: %v", step, err)
		}
		if got != want {
			t.Fatalf("%s: default background %+v, want %+v", step, got, want)
		}
	}
	host.Apply(Directive{Op: OpBackground, PageID: "page-1", Bg: "#1e1e2e"})
	readBackground("hidden, no bounds")
	host.Apply(Directive{Op: OpBounds, PageID: "page-1", X: 0, Y: 0, W: 640, H: 480})
	readBackground("after bounds")

	cdp := newSmokeCDP(t, smokeDialCDP(t, host.CDPPort()))
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	cdp.call("Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, "", &attached)
	// A document with no background of its own shows the base. (about:blank
	// is no witness: under a dark OS color scheme Chromium paints its own
	// dark canvas there, whatever the default background.)
	cdp.call("Page.enable", map[string]any{}, attached.SessionID, nil)
	cdp.call("Page.navigate", map[string]any{"url": "data:text/html,<title>t</title>"}, attached.SessionID, nil)
	var got string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		var shot struct {
			Data string `json:"data"`
		}
		cdp.call("Page.captureScreenshot", map[string]any{"format": "png"}, attached.SessionID, &shot)
		raw, err := base64.StdEncoding.DecodeString(shot.Data)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("decode the capture: %v", err)
		}
		b := img.Bounds()
		r, g, bl, _ := img.At(b.Min.X+b.Dx()/2, b.Min.Y+b.Dy()/2).RGBA()
		if got = fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, bl>>8); got == "#1e1e2e" {
			return
		}
	}
	t.Fatalf("the hidden page's document is captured on %s, want the base #1e1e2e", got)
}

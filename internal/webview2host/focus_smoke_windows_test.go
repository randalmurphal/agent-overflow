//go:build windows

package webview2host

import (
	"os"
	"testing"
	"time"
)

// The real WebView2 focus path runs only under the native browser smoke
// gate: it needs an installed WebView2 runtime and opens a window.
//
//	AO_NATIVE_BROWSER_SMOKE=1 go test -run TestFocusSmoke ./internal/webview2host
//
// OpFocus moves keyboard focus into a shown page while the smoke window is
// the foreground window, leaves it alone while another window is, and never
// focuses a hidden page.
func TestFocusSmokeMovesFocusIntoAShownPage(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	dataDir, err := os.MkdirTemp("", "ao-focus-smoke-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for start := time.Now(); ; time.Sleep(200 * time.Millisecond) {
			err := os.RemoveAll(dataDir)
			if err == nil {
				return
			}
			if time.Since(start) > 15*time.Second {
				t.Errorf("remove %s: %v", dataDir, err)
				return
			}
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
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		host.Close()
		ui.do(host.releaseEnvironment)
	})

	host.Apply(Directive{Op: OpCreate, PageID: "page-1", ProfileID: "smoke"})
	for created := false; !created; {
		select {
		case report := <-reports:
			switch report.kind {
			case ReportCreated:
				created = true
			case ReportFindUnavailable:
			default:
				t.Fatalf("create answered %+v", report)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("no created report")
		}
	}
	host.mu.Lock()
	page := host.pages["page-1"]
	host.mu.Unlock()
	focused := func() (inPage, foreground bool) {
		ui.do(func() {
			inPage = focusWithin(page.raiseTarget())
			foreground = isForeground(ui.hwnd)
		})
		return inPage, foreground
	}
	noFocusFailure := func(state string) {
		t.Helper()
		select {
		case report := <-reports:
			if report.kind == ReportFocusFailed {
				t.Fatalf("%s: the controller refused the focus: %s", state, report.detail)
			}
		default:
		}
	}

	// Hidden: never focused.
	host.Apply(Directive{Op: OpBounds, PageID: "page-1", X: 0, Y: 0, W: 640, H: 480})
	host.Apply(Directive{Op: OpFocus, PageID: "page-1"})
	if inPage, _ := focused(); inPage {
		t.Fatal("a page never shown took the focus")
	}

	host.Apply(Directive{Op: OpShow, PageID: "page-1"})
	host.Apply(Directive{Op: OpFocus, PageID: "page-1"})
	time.Sleep(500 * time.Millisecond)
	inPage, foreground := focused()
	noFocusFailure("shown")
	if foreground && !inPage {
		t.Fatal("a shown page in the foreground window did not take the focus")
	}
	if !foreground && inPage {
		t.Fatal("a focus moved into the page while another application's window was in front")
	}
	t.Logf("shown: the smoke window is in the foreground: %v; the page holds the focus: %v", foreground, inPage)

	// A hide gives the focus back; a focus asked for after it takes none.
	host.Apply(Directive{Op: OpHide, PageID: "page-1"})
	host.Apply(Directive{Op: OpFocus, PageID: "page-1"})
	time.Sleep(500 * time.Millisecond)
	if inPage, _ := focused(); inPage {
		t.Fatal("a hidden page took the focus")
	}
	noFocusFailure("hidden")
}

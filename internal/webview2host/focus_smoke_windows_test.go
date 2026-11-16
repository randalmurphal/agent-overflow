//go:build windows

package webview2host

import (
	"os"
	"testing"
	"time"
)

var procSetForegroundWindow = user32.NewProc("SetForegroundWindow")

// The real WebView2 focus path runs only under the native browser smoke
// gate: it needs an installed WebView2 runtime and opens a window.
//
//	AO_NATIVE_BROWSER_SMOKE=1 go test -run TestFocusSmoke ./internal/webview2host
//
// With the smoke window established as the foreground window, OpFocus moves
// keyboard focus from the app into a shown page, and a page hidden when its
// directive arrives takes it when next shown; it leaves focus alone for a
// withdrawn intent, pending or not, for a hidden page, and while another
// window is in the foreground.
// Windows grants the foreground only to a process allowed to take it, so a
// run that cannot establish it skips, saying so, rather than pass on
// assertions it could not make.
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
	intents := NewFocusIntents()
	host, err := New(Config{
		HostWindow:  func() uintptr { return ui.hwnd },
		UserDataDir: dataDir,
		Logf:        t.Logf,
		Report: func(pageID string, kind ReportKind, detail string) {
			reports <- smokeReport{pageID, kind, detail}
		},
		OnMain: ui.do,
		Focus:  intents,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		host.Close()
		ui.do(host.env.release)
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
	inPage := func() (held bool) {
		ui.do(func() { held = focusWithin(page.raiseTarget()) })
		return held
	}
	// waitInPage polls for the page to hold the focus, or for the time
	// within which a move would have landed to pass.
	waitInPage := func(want bool) bool {
		deadline := time.Now().Add(3 * time.Second)
		for {
			if held := inPage(); held == want || time.Now().After(deadline) {
				return held
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	// noReport asserts the host answered nothing: a declined focus is not
	// reported.
	noReport := func(state string) {
		t.Helper()
		select {
		case report := <-reports:
			t.Fatalf("%s: the host reported %+v", state, report)
		default:
		}
	}
	focusReport := func(state string, intent uint64) {
		t.Helper()
		select {
		case report := <-reports:
			parsed, err := ParseFocusReport(report.detail)
			if report.kind != ReportFocused || err != nil || parsed.Intent != intent {
				t.Fatalf("%s: the host reported %+v (%v), want the focus of intent %d", state, report, err, intent)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no focus report", state)
		}
	}
	focus := func(intent uint64) {
		host.Apply(Directive{Op: OpFocus, PageID: "page-1", Mount: "mount-1", Intent: intent})
	}
	arm := func(intent uint64, withdrawn bool) {
		intents.Note(FocusIntentMessage{Mount: "mount-1", Intent: intent, Withdrawn: withdrawn})
	}
	// toApp gives the smoke window, standing in for the SPA, the keyboard.
	toApp := func(state string) {
		t.Helper()
		var ok bool
		ui.do(func() { ok = focusWindow(ui.hwnd) })
		if !ok || waitInPage(false) {
			t.Fatalf("%s: the smoke window could not take the keyboard back", state)
		}
	}

	host.Apply(Directive{Op: OpBounds, PageID: "page-1", X: 0, Y: 0, W: 640, H: 480})
	var foreground bool
	ui.do(func() {
		_, _, _ = procSetForegroundWindow.Call(ui.hwnd)
		foreground = isForeground(ui.hwnd)
	})
	if !foreground {
		t.Skip("Windows did not make the smoke window the foreground window (a process it does not allow to take the foreground); run the smoke from the foreground console")
	}
	toApp("the app holds the keyboard")

	// Hidden: not focused, and the intent waits for the page's next show.
	arm(1, false)
	focus(1)
	if waitInPage(true) {
		t.Fatal("a page never shown took the focus")
	}
	noReport("hidden")
	host.Apply(Directive{Op: OpShow, PageID: "page-1"})
	if !waitInPage(true) {
		t.Fatal("the page's show did not apply its waiting focus")
	}
	focusReport("shown", 1)

	// From the app into a shown page.
	toApp("before a shown page's focus")
	arm(2, false)
	focus(2)
	if !waitInPage(true) {
		t.Fatal("a shown page in the foreground window did not take the focus")
	}
	focusReport("focused", 2)

	// A withdrawn intent moves nothing: the user chose elsewhere while the
	// directive was on its way, or while it waited for the page's show.
	toApp("before a withdrawn intent")
	arm(3, false)
	arm(3, true)
	focus(3)
	if waitInPage(true) {
		t.Fatal("a withdrawn intent moved the focus into the page")
	}
	noReport("withdrawn")
	host.Apply(Directive{Op: OpHide, PageID: "page-1"})
	arm(4, false)
	focus(4)
	arm(4, true)
	host.Apply(Directive{Op: OpShow, PageID: "page-1"})
	if waitInPage(true) {
		t.Fatal("a waiting intent withdrawn before the show moved the focus")
	}
	noReport("withdrawn while waiting")

	t.Run("another window in the foreground", func(t *testing.T) {
		var other uintptr
		var otherForeground bool
		ui.do(func() {
			window, err := createSmokeWindow()
			if err != nil {
				return
			}
			other = window
			_, _, _ = procSetForegroundWindow.Call(other)
			otherForeground = isForeground(other) && !isForeground(ui.hwnd)
		})
		if other == 0 {
			t.Fatal("create a second window")
		}
		t.Cleanup(func() { ui.do(func() { destroyWindow(other) }) })
		if !otherForeground {
			t.Skip("Windows did not give the second window the foreground")
		}
		arm(5, false)
		focus(5)
		if waitInPage(true) {
			t.Fatal("a focus moved into the page while another window was in front")
		}
		var stillOther bool
		ui.do(func() { stillOther = isForeground(other) })
		if !stillOther {
			t.Fatal("the focus took the foreground from the other window")
		}
		noReport("another window in front")
	})

	// A hide gives the focus back; a focus asked for after it takes none.
	ui.do(func() { _, _, _ = procSetForegroundWindow.Call(ui.hwnd) })
	host.Apply(Directive{Op: OpHide, PageID: "page-1"})
	arm(6, false)
	focus(6)
	if waitInPage(true) {
		t.Fatal("a hidden page took the focus")
	}
	noReport("hidden again")
}

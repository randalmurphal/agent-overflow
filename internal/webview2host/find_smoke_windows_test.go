//go:build windows

package webview2host

import (
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The real WebView2 find path runs only under the native browser smoke gate:
// it needs an installed WebView2 runtime and opens a window.
//
//	AO_NATIVE_BROWSER_SMOKE=1 go test -run TestFindSmoke ./internal/webview2host
func TestFindSmokeOpensTheEnginesFindBar(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	// The runtime's browser process holds the user-data folder for a moment
	// after its last page closes, which t.TempDir's cleanup does not retry.
	dataDir, err := os.MkdirTemp("", "ao-find-smoke-")
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
				t.Fatalf("the installed runtime reported no find API")
			default:
				t.Fatalf("create answered %+v", report)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("no created report")
		}
	}
	host.Apply(Directive{Op: OpBounds, PageID: "page-1", X: 0, Y: 0, W: 640, H: 480})
	host.Apply(Directive{Op: OpShow, PageID: "page-1"})

	host.mu.Lock()
	page := host.pages["page-1"]
	host.mu.Unlock()
	ui.do(func() {
		html := "<p>alpha beta</p><p>gamma ALPHA delta</p>"
		if err := smokeNavigateToString(page.view, html); err != nil {
			t.Errorf("NavigateToString: %v", err)
		}
	})

	// The document loads asynchronously; a find started before it does counts
	// nothing, so the find restarts until the count settles.
	deadline := time.Now().Add(20 * time.Second)
	var count, active int32
	for time.Now().Before(deadline) {
		host.Apply(Directive{Op: OpFind, PageID: "page-1", Query: "alpha"})
		time.Sleep(500 * time.Millisecond)
		ui.do(func() { count, active = smokeFindState(t, page) })
		if count == 2 {
			break
		}
	}
	// ActiveMatchIndex starts at 1 for the first match (WebView2.idl).
	if count != 2 || active != 1 {
		t.Fatalf("find for alpha: count %d, active index %d; want 2 matches, the first active", count, active)
	}

	t.Logf("find for alpha: %d matches, active index %d", count, active)

	host.Apply(Directive{Op: OpFindStop, PageID: "page-1"})
	host.Apply(Directive{Op: OpFind, PageID: "page-1", Query: "absent"})
	time.Sleep(time.Second)
	ui.do(func() { count, _ = smokeFindState(t, page) })
	if count != 0 {
		t.Fatalf("find for absent: count %d", count)
	}
	host.Apply(Directive{Op: OpFindStop, PageID: "page-1"})
}

type smokeReport struct {
	pageID string
	kind   ReportKind
	detail string
}

// smokeFindState reads get_MatchCount and get_ActiveMatchIndex. Runs on the
// UI thread.
func smokeFindState(t *testing.T, page *hostPage) (count, active int32) {
	find, err := page.view.pageFind()
	if err != nil || find == nil {
		t.Errorf("page find: %v (nil=%v)", err, find == nil)
		return -1, -1
	}
	defer find.release()
	count, err = find.matchCount()
	if err != nil {
		t.Errorf("get_MatchCount: %v", err)
	}
	// get_ActiveMatchIndex([out, retval] INT32* value), ICoreWebView2FindVtbl.
	hr, _, _ := find.vtbl.GetActiveMatchIndex.Call(uintptr(unsafe.Pointer(find)), uintptr(unsafe.Pointer(&active)))
	if err := hresult(hr); err != nil {
		t.Errorf("get_ActiveMatchIndex: %v", err)
	}
	return count, active
}

// smokeNavigateToString is ICoreWebView2::NavigateToString([in] LPCWSTR).
func smokeNavigateToString(view *iCoreWebView2, html string) error {
	ptr, err := windows.UTF16PtrFromString(html)
	if err != nil {
		return err
	}
	hr, _, _ := view.vtbl.NavigateToString.Call(uintptr(unsafe.Pointer(view)), uintptr(unsafe.Pointer(ptr)))
	return hresult(hr)
}

var (
	procPeekMessage      = user32.NewProc("PeekMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessage  = user32.NewProc("DispatchMessageW")
)

const (
	smokeClassName       = "AOFindSmokeWindow"
	wsOverlappedWindow   = 0x00CF0000
	wsVisible            = 0x10000000
	pmRemove             = 0x0001
	smokeMessageWords    = 8
	smokeWindowDimension = 800
)

// smokeUIThread is a locked OS thread with an STA, a top-level window and a
// message pump that also runs queued functions.
type smokeUIThread struct {
	hwnd  uintptr
	queue chan func()
}

func (u *smokeUIThread) do(fn func()) {
	done := make(chan struct{})
	u.queue <- func() { defer close(done); fn() }
	<-done
}

func startSmokeUIThread(t *testing.T) *smokeUIThread {
	t.Helper()
	ui := &smokeUIThread{queue: make(chan func(), 64)}
	ready := make(chan error, 1)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil {
			ready <- err
			return
		}
		defer windows.CoUninitialize()
		hwnd, err := createSmokeWindow()
		if err != nil {
			ready <- err
			return
		}
		defer destroyWindow(hwnd)
		ui.hwnd = hwnd
		ready <- nil
		var msg [smokeMessageWords]uint64 // MSG is 48 bytes on 64-bit
		for {
			select {
			case <-stop:
				return
			case fn := <-ui.queue:
				fn()
			default:
			}
			for {
				got, _, _ := procPeekMessage.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0, pmRemove)
				if got == 0 {
					break
				}
				_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg[0])))
				_, _, _ = procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg[0])))
			}
			time.Sleep(time.Millisecond)
		}
	}()
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(stop); wg.Wait() })
	return ui
}

func createSmokeWindow() (uintptr, error) {
	var module windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &module); err != nil {
		return 0, err
	}
	name, err := windows.UTF16PtrFromString(smokeClassName)
	if err != nil {
		return 0, err
	}
	class := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   clipWndProc,
		hInstance:     module,
		lpszClassName: name,
	}
	if atom, _, callErr := procRegisterClass.Call(uintptr(unsafe.Pointer(&class))); atom == 0 && !isClassAlreadyExists(callErr) {
		return 0, callErr
	}
	hwnd, _, callErr := procCreateWindowEx.Call(
		0, uintptr(unsafe.Pointer(name)), 0,
		wsOverlappedWindow|wsVisible,
		0, 0, smokeWindowDimension, smokeWindowDimension,
		0, 0, uintptr(module), 0,
	)
	if hwnd == 0 {
		return 0, callErr
	}
	return hwnd, nil
}

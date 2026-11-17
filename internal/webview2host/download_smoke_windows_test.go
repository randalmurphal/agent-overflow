//go:build windows

package webview2host

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The launcher owns a pane page's downloads (download_windows.go). This runs
// that path on the real runtime, off screen under the window watcher, and
// proves for downloads a page starts:
//
//   - each lands in the user's Downloads folder under its own name, a taken
//     name becoming "name (1).ext", and the started report already names the
//     final file, which keeps WebView2's mark of the web; a 50 MB download
//     reports its progress at most once per DownloadProgressInterval;
//   - a Content-Disposition navigation, an anchor's download attribute and a
//     blob: URL are all taken;
//   - WebView2's download dialog never opens (IsDefaultDownloadDialogOpen
//     stays 0) and nothing appears on screen;
//   - OpCancelDownload ends a running download as interrupted, canceled,
//     and leaves no file; a server that drops the connection ends one as
//     interrupted with the network reason, after WebView2's own retries, as
//     one download with one terminal report, and leaves no file;
//   - a second download a page's script starts without a gesture raises the
//     multiple-downloads permission request before it starts; once allowed it
//     is taken like any other.
//
// Every file it makes carries a per-run prefix, and exactly those files are
// deleted from the Downloads folder when it ends.
//
//	AO_NATIVE_BROWSER_SMOKE=1 go test -run TestDownloadSmoke ./internal/webview2host
func TestDownloadSmokeSavesToTheDownloadsFolderWithNoDialog(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	s := startDownloadSmoke(t)

	first := s.navigateDownload("small.bin", "/cd/small.bin?size=4096")
	s.expectCompleted(first, "small.bin", 4096)
	firstInfo, err := os.Stat(first.path)
	if err != nil {
		t.Fatal(err)
	}
	// WebView2 marks a download as from the web, and nothing removes it.
	if mark, err := os.ReadFile(first.path + ":Zone.Identifier"); err != nil || !strings.Contains(string(mark), "ZoneId=") {
		t.Fatalf("the download carries no mark of the web: %q, %v", mark, err)
	} else {
		t.Logf("mark of the web: %q", mark)
	}

	again := s.navigateDownload("small.bin", "/cd/small.bin?size=4096")
	s.expectCompleted(again, "small (1).bin", 4096)
	if now, err := os.Stat(first.path); err != nil || !os.SameFile(now, firstInfo) || !now.ModTime().Equal(firstInfo.ModTime()) {
		t.Fatalf("the second download touched the first file: %v", err)
	}

	start := time.Now()
	big := s.navigateDownload("big.bin", "/cd/big.bin?size=52428800")
	s.expectCompleted(big, "big.bin", 52428800)
	if limit := int(time.Since(start)/DownloadProgressInterval) + 2; big.progress > limit {
		t.Fatalf("50 MB reported progress %d times in %v, over the bound %d", big.progress, time.Since(start), limit)
	}

	held := s.navigateStarted("slow.bin", "/slow/slow.bin")
	s.host.Apply(Directive{Op: OpCancelDownload, PageID: "page-1", Download: held.id})
	s.settle(held)
	if held.final.State != DownloadInterrupted || held.final.Reason != downloadInterruptReason(downloadUserCanceled) {
		t.Fatalf("the canceled download ended %+v", held.final)
	}
	s.expectNoFile("slow.bin")

	failed := s.navigateStarted("fail.bin", "/fail/fail.bin")
	s.settle(failed)
	if failed.final.State != DownloadInterrupted || !strings.Contains(failed.final.Reason, "network") && !strings.Contains(failed.final.Reason, "server") {
		t.Fatalf("the dropped download ended %+v", failed.final)
	}
	// WebView2 retried it: each retry is a new DownloadStarting and
	// operation for the same file, which the launcher keeps as this one.
	if n := s.requests("fail.bin"); n < 2 {
		t.Fatalf("the dropped download was requested %d times; the retry this smoke covers did not happen", n)
	}
	t.Logf("a dropped download: %d requests, ended %q", s.requests("fail.bin"), failed.final.Reason)
	// WebView2 retries a dropped download on its own; the retries must not
	// look like more downloads.
	if extra := s.quietFor(3*time.Second, "fail.bin"); len(extra) != 0 {
		t.Fatalf("after the end, reports for the same address: %+v", extra)
	}
	s.expectNoFile("fail.bin")

	s.navigate("/page/anchor.bin")
	s.evaluate(`document.getElementById("a").click()`)
	anchor := s.started("/raw/x")
	s.settle(anchor)
	s.expectCompleted(anchor, "anchor.bin", 2048)

	s.navigate("/page/none")
	s.evaluate(fmt.Sprintf(`(() => { const a = document.createElement("a"); a.href = URL.createObjectURL(new Blob(["hello blob"], {type: "text/plain"})); a.download = %q; document.body.appendChild(a); a.click() })()`, s.prefix+"blob.txt"))
	blob := s.started("blob:")
	s.settle(blob)
	s.expectCompleted(blob, "blob.txt", 10)

	// A second download the page's script starts with no gesture asks the
	// multiple-downloads permission first. The launcher does not handle the
	// request, so WebView2 shows its own prompt; the smoke answers every
	// request itself (shown, a prompt would be on screen) and the download
	// is then taken.
	before := len(s.permissionKinds())
	s.evaluate(fmt.Sprintf(`(() => { const a = document.createElement("a"); a.href = URL.createObjectURL(new Blob(["second"], {type: "text/plain"})); a.download = %q; document.body.appendChild(a); a.click() })()`, s.prefix+"second.txt"))
	second := s.started("blob:")
	s.settle(second)
	s.expectCompleted(second, "second.txt", 6)
	kinds := s.permissionKinds()
	t.Logf("permission requests over the smoke: %v", kinds)
	if len(kinds) != before+1 || kinds[before] != permissionMultipleDownloads {
		t.Fatalf("the second scripted download raised %v, want one multiple-downloads request", kinds[before:])
	}
}

// downloadSmoke is one off-screen page whose downloads the smoke drives.
type downloadSmoke struct {
	t         *testing.T
	ui        *smokeUIThread
	host      *Host
	cdp       *smokeCDP
	session   string
	server    *httptest.Server
	reports   chan smokeReport
	prefix    string
	downloads string
	hits      map[string]int
	hitsMu    chan struct{}
	perm      *smokePermissions
}

// smokeDownload is what the launcher reported of one download.
type smokeDownload struct {
	id       string
	uri      string
	path     string
	progress int
	final    DownloadReport
}

func startDownloadSmoke(t *testing.T) *downloadSmoke {
	downloads, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := &downloadSmoke{
		t: t, downloads: downloads, prefix: "ao-download-smoke-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-",
		reports: make(chan smokeReport, 4096), hits: map[string]int{}, hitsMu: make(chan struct{}, 1),
	}
	partialsBefore := s.partials()
	// Registered first, so it runs last: after the host is gone and every
	// file is closed.
	t.Cleanup(func() {
		matches, _ := filepath.Glob(filepath.Join(downloads, s.prefix+"*"))
		for _, match := range matches {
			if err := os.Remove(match); err != nil {
				t.Errorf("remove %s: %v", match, err)
			}
		}
		for name := range s.partials() {
			if !partialsBefore[name] {
				t.Errorf("a partial download file appeared and stayed: %s (left alone: its name does not say whose it is)", name)
			}
		}
	})
	dataDir, err := os.MkdirTemp("", "ao-download-smoke-")
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

	release := make(chan struct{})
	s.server = httptest.NewServer(http.HandlerFunc(s.serve(release)))
	t.Cleanup(s.server.Close)
	t.Cleanup(func() { close(release) })

	s.ui = startSmokeUIThread(t)
	s.host, err = New(Config{
		HostWindow:  func() uintptr { return s.ui.hwnd },
		UserDataDir: dataDir,
		Logf:        t.Logf,
		Report: func(pageID string, kind ReportKind, detail string) {
			s.reports <- smokeReport{pageID, kind, detail}
		},
		OnMain: s.ui.do,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.host.Close()
		s.ui.do(s.host.env.release)
	})
	s.host.Apply(Directive{Op: OpCreate, PageID: "page-1", ProfileID: "smoke"})
	var target string
	for target == "" {
		select {
		case report := <-s.reports:
			switch report.kind {
			case ReportCreated:
				target = report.detail
			case ReportFindUnavailable:
			default:
				t.Fatalf("create answered %+v", report)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("no created report")
		}
	}
	s.host.Apply(Directive{Op: OpBounds, PageID: "page-1", X: 0, Y: 0, W: 640, H: 480})
	s.host.Apply(Directive{Op: OpShow, PageID: "page-1"})
	s.perm = watchSmokePermissions(t, s.ui, s.page())
	s.permissions(permissionAllow)
	s.cdp = newSmokeCDP(t, smokeDialCDP(t, s.host.CDPPort()))
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	s.cdp.call("Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}, "", &attached)
	s.session = attached.SessionID
	return s
}

func (s *downloadSmoke) page() *hostPage {
	s.host.mu.Lock()
	defer s.host.mu.Unlock()
	return s.host.pages["page-1"]
}

// serve answers /cd/<name>?size=n as an attachment, /slow/<name> as one
// that holds until the smoke ends, /fail/<name> as one whose connection
// drops, and /page/<name> as a page with an anchor downloading <name>.
func (s *downloadSmoke) serve(release chan struct{}) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		kind, name, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		s.hitsMu <- struct{}{}
		s.hits[name]++
		<-s.hitsMu
		switch kind {
		case "page":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<html><body><a id="a" href="/raw/x" download="%s">a</a></body></html>`, s.prefix+name)
			return
		case "raw":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(make([]byte, 2048))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+s.prefix+name+`"`)
		switch kind {
		case "cd":
			size, _ := strconv.Atoi(r.URL.Query().Get("size"))
			w.Header().Set("Content-Length", strconv.Itoa(size))
			chunk := make([]byte, 1<<20)
			for size > 0 {
				n := min(size, len(chunk))
				if _, err := w.Write(chunk[:n]); err != nil {
					return
				}
				size -= n
			}
		case "slow":
			w.Header().Set("Content-Length", "1048576")
			_, _ = w.Write(make([]byte, 1024))
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
		case "fail":
			w.Header().Set("Content-Length", "1048576")
			_, _ = w.Write(make([]byte, 4096))
			w.(http.Flusher).Flush()
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	}
}

func (s *downloadSmoke) requests(name string) int {
	s.hitsMu <- struct{}{}
	defer func() { <-s.hitsMu }()
	return s.hits[name]
}

func (s *downloadSmoke) navigate(path string) {
	s.t.Helper()
	s.cdp.call("Page.navigate", map[string]any{"url": s.server.URL + path}, s.session, nil)
	// The anchor and blob cases click in the new document.
	time.Sleep(500 * time.Millisecond)
}

func (s *downloadSmoke) evaluate(expression string) {
	s.t.Helper()
	s.cdp.call("Runtime.evaluate", map[string]any{"expression": expression, "userGesture": true}, s.session, nil)
}

// navigateStarted navigates to a download and answers it once started.
func (s *downloadSmoke) navigateStarted(name, path string) *smokeDownload {
	s.t.Helper()
	s.cdp.call("Page.navigate", map[string]any{"url": s.server.URL + path}, s.session, nil)
	return s.started(name)
}

// navigateDownload navigates to a download and answers it once ended.
func (s *downloadSmoke) navigateDownload(name, path string) *smokeDownload {
	s.t.Helper()
	d := s.navigateStarted(name, path)
	s.settle(d)
	return d
}

// started waits for the next started report whose address contains match.
func (s *downloadSmoke) started(match string) *smokeDownload {
	s.t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case report := <-s.reports:
			r, ok := s.downloadReport(report)
			if !ok {
				continue
			}
			if r.State != DownloadStarted || !strings.Contains(r.URI, match) {
				s.t.Fatalf("waiting for a download of %s, the launcher reported %+v", match, r)
			}
			if !strings.EqualFold(filepath.Dir(r.Path), s.downloads) {
				s.t.Fatalf("the download of %s is to be saved at %s, outside %s", match, r.Path, s.downloads)
			}
			s.expectNoDialog()
			return &smokeDownload{id: r.ID, uri: r.URI, path: r.Path}
		case <-deadline:
			s.t.Fatalf("no download of %s began", match)
		}
	}
}

// settle reads d's reports to its end.
func (s *downloadSmoke) settle(d *smokeDownload) {
	s.t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case report := <-s.reports:
			r, ok := s.downloadReport(report)
			if !ok {
				continue
			}
			if r.ID != d.id {
				s.t.Fatalf("waiting for %s to end, the launcher reported %+v", d.id, r)
			}
			if r.Path != d.path && r.State != DownloadCompleted {
				s.t.Fatalf("%s moved from %s to %s before it ended", d.id, d.path, r.Path)
			}
			switch r.State {
			case DownloadProgress:
				d.progress++
			case DownloadCompleted, DownloadInterrupted:
				d.final, d.path = r, r.Path
				s.expectNoDialog()
				return
			default:
				s.t.Fatalf("%s reported %+v after it started", d.id, r)
			}
		case <-deadline:
			s.t.Fatalf("%s never ended", d.id)
		}
	}
}

// quietFor answers every download report naming match in its address that
// arrives within d.
func (s *downloadSmoke) quietFor(d time.Duration, match string) []DownloadReport {
	var out []DownloadReport
	deadline := time.After(d)
	for {
		select {
		case report := <-s.reports:
			if r, ok := s.downloadReport(report); ok && (strings.Contains(r.URI, match) || r.State != DownloadStarted) {
				out = append(out, r)
			}
		case <-deadline:
			return out
		}
	}
}

func (s *downloadSmoke) downloadReport(report smokeReport) (DownloadReport, bool) {
	s.t.Helper()
	if report.kind != ReportDownload {
		return DownloadReport{}, false
	}
	if report.pageID != "page-1" {
		s.t.Fatalf("a download report for page %s", report.pageID)
	}
	r, err := ParseDownloadReport(report.detail)
	if err != nil {
		s.t.Fatalf("download report %q: %v", report.detail, err)
	}
	return r, true
}

func (s *downloadSmoke) expectCompleted(d *smokeDownload, name string, size int64) {
	s.t.Helper()
	if d.final.State != DownloadCompleted {
		s.t.Fatalf("%s ended %+v", name, d.final)
	}
	if want := filepath.Join(s.downloads, s.prefix+name); !strings.EqualFold(d.path, want) {
		s.t.Fatalf("%s was saved as %s, want %s", name, d.path, want)
	}
	info, err := os.Stat(d.path)
	if err != nil || info.Size() != size || d.final.Received != size {
		s.t.Fatalf("%s at %s: %v, %d bytes on disk, %d reported, want %d", name, d.path, err, info.Size(), d.final.Received, size)
	}
}

// expectNoFile asserts nothing with the name, finished or partial, is left.
func (s *downloadSmoke) expectNoFile(name string) {
	s.t.Helper()
	time.Sleep(time.Second)
	matches, _ := filepath.Glob(filepath.Join(s.downloads, s.prefix+strings.TrimSuffix(name, filepath.Ext(name))+"*"))
	if len(matches) != 0 {
		s.t.Fatalf("%s left %v", name, matches)
	}
}

// partials answers the in-progress files Chromium names before a download's
// name is final.
func (s *downloadSmoke) partials() map[string]bool {
	matches, _ := filepath.Glob(filepath.Join(s.downloads, "Unconfirmed *.crdownload"))
	out := make(map[string]bool, len(matches))
	for _, match := range matches {
		out[filepath.Base(match)] = true
	}
	return out
}

// expectNoDialog asserts WebView2's own download dialog is closed.
func (s *downloadSmoke) expectNoDialog() {
	s.t.Helper()
	var open int32
	var err error
	s.ui.do(func() { open, err = smokeDownloadDialogOpen(s.page().view) })
	if err != nil {
		s.t.Fatalf("IsDefaultDownloadDialogOpen: %v", err)
	}
	if open != 0 {
		s.t.Fatal("WebView2's download dialog opened")
	}
}

func (s *downloadSmoke) permissions(state uint32) {
	s.ui.do(func() { s.perm.answer = state })
}

func (s *downloadSmoke) permissionKinds() []uint32 {
	var kinds []uint32
	s.ui.do(func() { kinds = append(kinds, s.perm.kinds...) })
	return kinds
}

// ---------------------------------------------------------------------
// ICoreWebView2_9 (the default download dialog), for the smoke alone
// ---------------------------------------------------------------------

// smokeWebView9Vtbl is the flattened chain through ICoreWebView2_9: 61-87
// are ICoreWebView2_2 through ICoreWebView2_8 (7+5+4+2+1+1+7), and 90 is
// get_IsDefaultDownloadDialogOpen.
type smokeWebView9Vtbl struct {
	iCoreWebView2Vtbl
	_                              [27]comProc
	AddDialogOpenChanged           comProc
	RemoveDialogOpenChanged        comProc
	GetIsDefaultDownloadDialogOpen comProc
}

type smokeWebView9 struct {
	vtbl *smokeWebView9Vtbl
}

// smokeIIDWebView2_9 is IID_ICoreWebView2_9, {4d7b2eab-9fdc-468d-b998-a9260b5ed651}.
var smokeIIDWebView2_9 = windows.GUID{
	Data1: 0x4d7b2eab, Data2: 0x9fdc, Data3: 0x468d,
	Data4: [8]byte{0xb9, 0x98, 0xa9, 0x26, 0x0b, 0x5e, 0xd6, 0x51},
}

func smokeDownloadDialogOpen(view *iCoreWebView2) (int32, error) {
	var view9 *smokeWebView9
	_, _, _ = view.vtbl.QueryInterface.Call(uintptr(unsafe.Pointer(view)), uintptr(unsafe.Pointer(&smokeIIDWebView2_9)), uintptr(unsafe.Pointer(&view9)))
	if view9 == nil {
		return 0, fmt.Errorf("the runtime has no ICoreWebView2_9")
	}
	defer func() { _, _, _ = view9.vtbl.Release.Call(uintptr(unsafe.Pointer(view9))) }()
	var open int32
	hr, _, _ := view9.vtbl.GetIsDefaultDownloadDialogOpen.Call(uintptr(unsafe.Pointer(view9)), uintptr(unsafe.Pointer(&open)))
	return open, hresult(hr)
}

// ---------------------------------------------------------------------
// PermissionRequested, for the smoke alone
// ---------------------------------------------------------------------

// COREWEBVIEW2_PERMISSION_KIND_MULTIPLE_AUTOMATIC_DOWNLOADS and
// COREWEBVIEW2_PERMISSION_STATE.
const (
	permissionMultipleDownloads = 7
	permissionDefault           = 0
	permissionAllow             = 1
)

type smokePermissionArgsVtbl struct {
	iUnknownVtbl
	GetURI             comProc
	GetPermissionKind  comProc
	GetIsUserInitiated comProc
	GetState           comProc
	PutState           comProc
}

type smokePermissionArgs struct {
	vtbl *smokePermissionArgsVtbl
}

// smokePermissions records the page's permission requests and answers each
// with answer; permissionDefault leaves WebView2's own prompt to the user.
// UI thread only.
type smokePermissions struct {
	vtbl   *handlerVtbl
	ref    comRef
	answer uint32
	kinds  []uint32
}

var smokePermissionsVtbl = handlerVtbl{
	iUnknownVtbl: handlerIUnknown,
	Invoke: newComProc(func(this *smokePermissions, _ *iCoreWebView2, args *smokePermissionArgs) uintptr {
		var kind uint32
		_, _, _ = args.vtbl.GetPermissionKind.Call(uintptr(unsafe.Pointer(args)), uintptr(unsafe.Pointer(&kind)))
		this.kinds = append(this.kinds, kind)
		if this.answer != permissionDefault {
			_, _, _ = args.vtbl.PutState.Call(uintptr(unsafe.Pointer(args)), uintptr(this.answer))
		}
		return 0
	}),
}

func watchSmokePermissions(t *testing.T, ui *smokeUIThread, page *hostPage) *smokePermissions {
	t.Helper()
	handler := &smokePermissions{vtbl: &smokePermissionsVtbl}
	handler.ref.own()
	var err error
	ui.do(func() {
		var token eventRegistrationToken
		hr, _, _ := page.view.vtbl.AddPermissionRequested.Call(uintptr(unsafe.Pointer(page.view)), uintptr(unsafe.Pointer(handler)), uintptr(unsafe.Pointer(&token)))
		err = hresult(hr)
	})
	if err != nil {
		t.Fatalf("add_PermissionRequested: %v", err)
	}
	return handler
}

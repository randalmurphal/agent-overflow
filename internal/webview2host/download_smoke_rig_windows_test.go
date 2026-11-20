//go:build windows

package webview2host

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// waitRequests waits until name was requested n times.
func (s *downloadSmoke) waitRequests(name string, n int) {
	s.t.Helper()
	for deadline := time.Now().Add(60 * time.Second); s.requests(name) < n; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			s.t.Fatalf("%s was requested %d times, want %d", name, s.requests(name), n)
		}
	}
}

// settleAmong is settle, letting running reports of the other ids pass.
// settleAll waits for every one of ds to end, in any order, failing on a
// report of any other download.
func (s *downloadSmoke) settleAll(ds ...*smokeDownload) {
	s.t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		var running *smokeDownload
		for _, d := range ds {
			if d.final.ID == "" {
				running = d
			}
		}
		if running == nil {
			return
		}
		select {
		case report := <-s.reports:
			r, ok := s.downloadReport(report)
			if !ok {
				continue
			}
			i := slices.IndexFunc(ds, func(d *smokeDownload) bool { return d.id == r.ID })
			if i < 0 {
				s.t.Fatalf("waiting for %d downloads to end, the launcher reported %+v", len(ds), r)
			}
			d := ds[i]
			if r.Seq <= d.seq {
				s.t.Fatalf("%s reported snapshot %d after %d", d.id, r.Seq, d.seq)
			}
			d.seq = r.Seq
			if !r.Terminal() {
				d.progress++
				continue
			}
			d.final, d.path = r, r.Path
		case <-deadline:
			s.t.Fatalf("%s never ended", running.id)
		}
	}
}

func (s *downloadSmoke) settleAmong(d *smokeDownload, others ...string) {
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
				if !slices.Contains(others, r.ID) || r.State != DownloadRunning {
					s.t.Fatalf("waiting for %s to end, the launcher reported %+v", d.id, r)
				}
				continue
			}
			if r.Seq <= d.seq {
				s.t.Fatalf("%s reported snapshot %d after %d", d.id, r.Seq, d.seq)
			}
			d.seq = r.Seq
			if !r.Terminal() {
				d.progress++
				continue
			}
			d.final, d.path = r, r.Path
			return
		case <-deadline:
			s.t.Fatalf("%s never ended", d.id)
		}
	}
}

// expectBytes asserts the file at path is a dropped download's whole body.
func (s *downloadSmoke) expectBytes(path string, size int) {
	s.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || len(data) != size {
		s.t.Fatalf("%s: %d bytes, %v; want %d", path, len(data), err, size)
	}
	for i, b := range data {
		if b != droppedByte(i) {
			s.t.Fatalf("%s: byte %d is %d, want %d", path, i, b, droppedByte(i))
		}
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
	ranges    map[string][]string
	hitsMu    chan struct{}
	perm      *smokePermissions
	// pairGo lets the first /pair download finish, and twinGo the /twin
	// resumes.
	pairGo, twinGo chan struct{}
}

// smokeDownload is what the launcher reported of one download.
type smokeDownload struct {
	id       string
	uri      string
	path     string
	progress int
	final    DownloadReport
	// seq is the number of the last snapshot read.
	seq uint64
}

func startDownloadSmoke(t *testing.T) *downloadSmoke {
	downloads, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := &downloadSmoke{
		t: t, downloads: downloads, prefix: "ao-download-smoke-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-",
		reports: make(chan smokeReport, 4096), hits: map[string]int{}, ranges: map[string][]string{}, hitsMu: make(chan struct{}, 1),
		pairGo: make(chan struct{}), twinGo: make(chan struct{}),
	}
	partialsBefore := s.partials()
	// Registered first, so it runs last: after the host is gone and every
	// file is closed.
	t.Cleanup(func() {
		// WebView2's processes may hold a file a moment after the host goes.
		for start := time.Now(); ; time.Sleep(200 * time.Millisecond) {
			matches, _ := filepath.Glob(filepath.Join(downloads, s.prefix+"*"))
			var failed []error
			for _, match := range matches {
				if err := os.Remove(match); err != nil {
					failed = append(failed, err)
				}
			}
			if len(failed) == 0 {
				break
			}
			if time.Since(start) > 15*time.Second {
				t.Errorf("remove the smoke's files: %v", failed)
				break
			}
		}
		for name := range s.partials() {
			if !partialsBefore[name] {
				t.Errorf("a partial download file appeared and stayed: %s (left alone: its name does not say whose it is)", name)
			}
		}
	})
	dataDir := smokeTempDir(t, "ao-download-smoke-")

	release := make(chan struct{})
	s.server = httptest.NewServer(http.HandlerFunc(s.serve(release)))
	t.Cleanup(s.server.Close)
	t.Cleanup(func() { close(release) })

	s.ui = startSmokeUIThread(t)
	// Checked after the host's close, while the UI thread still pumps the
	// releases WebView2 makes.
	base := comRootCount()
	t.Cleanup(func() { awaitComRoots(t, base) })
	s.host, err = New(Config{
		HostWindow:  func() uintptr { return s.ui.hwnd },
		UserDataDir: dataDir,
		Logf:        t.Logf,
		Report: func(pageID string, kind ReportKind, detail string) {
			s.reports <- smokeReport{pageID, kind, detail}
		},
		OnMain: s.ui.do,
		Post:   s.ui.post,
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
			if report.kind == ReportCreated {
				target = report.detail
			} else if report.kind != ReportFindUnavailable {
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
	var page *hostPage
	s.ui.do(func() { page = s.host.pages["page-1"] })
	return page
}

// navigate opens path and waits until its document has loaded, since the
// anchor and blob cases click in the new document. An evaluation the
// navigation refuses, its document going, is asked again.
func (s *downloadSmoke) navigate(path string) {
	s.t.Helper()
	url := s.server.URL + path
	s.cdp.call("Page.navigate", map[string]any{"url": url}, s.session, nil)
	quoted, err := json.Marshal(url)
	if err != nil {
		s.t.Fatal(err)
	}
	loaded := fmt.Sprintf("location.href === %s && document.readyState === 'complete'", quoted)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var answer struct {
			Result struct {
				Value bool `json:"value"`
			} `json:"result"`
		}
		if refusal := s.cdp.try("Runtime.evaluate", map[string]any{"expression": loaded, "returnByValue": true}, s.session, &answer); refusal == "" && answer.Result.Value {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("%s did not load", url)
		}
	}
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
			if r.State != DownloadRunning || !strings.Contains(r.URI, match) {
				s.t.Fatalf("waiting for a download of %s, the launcher reported %+v", match, r)
			}
			if !strings.EqualFold(filepath.Dir(r.Path), s.downloads) {
				s.t.Fatalf("the download of %s is to be saved at %s, outside %s", match, r.Path, s.downloads)
			}
			s.expectNoDialog()
			return &smokeDownload{id: r.ID, uri: r.URI, path: r.Path, seq: r.Seq}
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
			if r.Path != d.path && r.State != DownloadCompleted || r.URI != d.uri {
				s.t.Fatalf("%s moved from %s (%s) to %s (%s) before it ended", d.id, d.uri, d.path, r.URI, r.Path)
			}
			if r.Seq <= d.seq {
				s.t.Fatalf("%s reported snapshot %d after %d", d.id, r.Seq, d.seq)
			}
			d.seq = r.Seq
			if !r.Terminal() {
				d.progress++
				continue
			}
			d.final, d.path = r, r.Path
			s.expectNoDialog()
			return
		case <-deadline:
			s.t.Fatalf("%s never ended", d.id)
		}
	}
}

// quietFor answers every download report that arrives within d.
func (s *downloadSmoke) quietFor(d time.Duration) []DownloadReport {
	var out []DownloadReport
	deadline := time.After(d)
	for {
		select {
		case report := <-s.reports:
			if r, ok := s.downloadReport(report); ok {
				out = append(out, r)
			}
		case <-deadline:
			return out
		}
	}
}

// nextReport answers the next download report.
func (s *downloadSmoke) nextReport() DownloadReport {
	s.t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case report := <-s.reports:
			if r, ok := s.downloadReport(report); ok {
				return r
			}
		case <-deadline:
			s.t.Fatal("no download report")
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

// smokeDownloadDialogOpen answers whether the view's default download
// dialog is open. UI thread.
func smokeDownloadDialogOpen(view *iCoreWebView2) (int32, error) {
	view9 := view.queryWebView9()
	if view9 == nil {
		return 0, fmt.Errorf("the runtime has no ICoreWebView2_9")
	}
	defer view9.release()
	open, err := view9.downloadDialogOpen()
	if open {
		return 1, err
	}
	return 0, err
}

// awaitComRoots waits for the Go-backed COM objects to return to base once
// WebView2 lets go of what the smoke handed it.
func awaitComRoots(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for comRootCount() != base {
		if time.Now().After(deadline) {
			t.Errorf("%d Go-backed COM objects are rooted after the smoke, want %d", comRootCount(), base)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

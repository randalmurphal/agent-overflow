//go:build windows

package webview2host

import (
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
	canceled := time.Now()
	s.host.Apply(Directive{Op: OpCancelDownload, PageID: "page-1", Download: held.id, Cancel: 1})
	s.settle(held)
	if held.final.State != DownloadInterrupted || held.final.Reason != downloadInterruptReason(downloadUserCanceled) || held.final.End != DownloadEndCanceled {
		t.Fatalf("the canceled download ended %+v", held.final)
	}
	// WebView2 ends a running download's cancel itself, well inside the
	// bound after which the launcher would.
	t.Logf("WebView2 ended the canceled download in %v", time.Since(canceled))
	if took := time.Since(canceled); took > downloadCancelBound/2 {
		t.Fatalf("WebView2 took %v to end a canceled download; the bound is %v", took, downloadCancelBound)
	}
	if extra := s.quietFor(downloadCancelBound + time.Second); len(extra) != 0 {
		t.Fatalf("after the cancel ended, more reports: %+v", extra)
	}
	s.expectNoFile("slow.bin")

	failed := s.navigateStarted("fail.bin", "/fail/fail.bin")
	s.settle(failed)
	if failed.final.State != DownloadInterrupted || failed.final.End != DownloadEndFailed || !strings.Contains(failed.final.Reason, "network") && !strings.Contains(failed.final.Reason, "server") {
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
	if extra := s.quietFor(3 * time.Second); len(extra) != 0 {
		t.Fatalf("after the end, more reports: %+v", extra)
	}
	s.expectNoFile("fail.bin")

	// A directive naming a download is answered with a snapshot marked as
	// an answer: an ended one's terminal snapshot again, whether it asks for
	// a cancel or the status, and an unknown id "unknown".
	kept := held.final
	kept.Answer = true
	for _, op := range []Op{OpCancelDownload, OpDownloadStatus} {
		s.host.Apply(Directive{Op: op, PageID: "page-1", Download: held.id, Cancel: 1})
		if got := s.nextReport(); got != kept {
			t.Fatalf("%s of an ended download answered %+v, want %+v", op, got, kept)
		}
	}
	s.host.Apply(Directive{Op: OpDownloadStatus, PageID: "page-1", Download: "dl-none-1"})
	if got := s.nextReport(); got != (DownloadReport{ID: "dl-none-1", State: DownloadUnknown, Reason: DownloadUnknownReason, Answer: true}) {
		t.Fatalf("an unknown id answered %+v", got)
	}
	// A running one answers its running snapshot, and a status cancels
	// nothing.
	asked := s.navigateStarted("asked.bin", "/slow/asked.bin")
	s.host.Apply(Directive{Op: OpDownloadStatus, PageID: "page-1", Download: asked.id})
	if got := s.nextReport(); got.ID != asked.id || got.State != DownloadRunning || got.Path != asked.path || !got.Answer {
		t.Fatalf("a status of a running download answered %+v", got)
	}
	for _, later := range s.quietFor(2 * time.Second) {
		if later.ID != asked.id || later.State != DownloadRunning || later.Path != asked.path {
			t.Fatalf("a status ended or moved the download: %+v", later)
		}
	}
	s.host.Apply(Directive{Op: OpCancelDownload, PageID: "page-1", Download: asked.id, Cancel: 1})
	s.settle(asked)
	if asked.final.State != DownloadInterrupted {
		t.Fatalf("the asked download ended %+v", asked.final)
	}
	s.expectNoFile("asked.bin")

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

// A download whose connection drops after part of its body is resumed by
// WebView2 itself as a new DownloadStarting, and the launcher reports it as
// the one download it started as, under its own name: from a server with
// ranges WebView2 continues from the bytes it has (Range bytes=65536-, the
// first attempt's bytes kept), from one that answers the Range request
// with the whole body it starts over (the first attempt's bytes dropped),
// and from one with no validator to resume against it restarts with no
// Range, proposing the download's own name. Each time the dropped
// operation reads as dropped when the resume starts, so the resume is
// adopted; the file is byte for byte the server's and no "name (1)" file
// appears. The resuming start and the dropped operation carry the
// interruption's reason (24, the length mismatch); no StateChanged reports
// an interruption first, and none of FILE_TOO_SHORT, FILE_HASH_MISMATCH and
// SERVER_NO_RANGE arrives.
func TestDownloadSmokeFollowsWebView2sOwnResume(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	s := startDownloadSmoke(t)
	for _, kind := range []string{"resume", "norange", "novalidator"} {
		name := kind + ".bin"
		d := s.navigateDownload(name, "/"+kind+"/"+name)
		t.Logf("%s: %d requests, ranges %q, %d running reports, ended %+v", kind, s.requests(name), s.rangesAsked(name), d.progress, d.final)
		s.expectCompleted(d, name, droppedSize)
		data, err := os.ReadFile(d.path)
		if err != nil {
			t.Fatal(err)
		}
		for i, b := range data {
			if b != droppedByte(i) {
				t.Fatalf("%s: byte %d of the resumed file is %d, want %d", kind, i, b, droppedByte(i))
			}
		}
		ranges := s.rangesAsked(name)
		if kind == "novalidator" {
			// Nothing to resume against: WebView2 restarts from nothing.
			if len(ranges) != 2 || ranges[0] != "" || ranges[1] != "" {
				t.Fatalf("%s: the requests asked for %q; the restart this smoke covers did not happen", kind, ranges)
			}
		} else if len(ranges) != 2 || ranges[0] != "" || ranges[1] != "bytes=65536-" {
			t.Fatalf("%s: the requests asked for %q; the resume this smoke covers did not happen", kind, ranges)
		}
		if matches, _ := filepath.Glob(filepath.Join(s.downloads, s.prefix+kind+"*")); len(matches) != 1 {
			t.Fatalf("%s: one click left %v", kind, matches)
		}
		if extra := s.quietFor(2 * time.Second); len(extra) != 0 {
			t.Fatalf("%s: after the end, more reports: %+v", kind, extra)
		}
	}
}

// The reviewer's sequence: two downloads from one address save to
// "pair.bin" and "pair (1).bin", and only the second drops. Its operation
// alone reads as dropped when WebView2's resume of it starts, so the
// resume is that download's: its row and its file, completed byte for
// byte, while the healthy first download keeps its row and file and
// completes byte for byte too, and no third file appears.
func TestDownloadSmokeAResumeReturnsToTheDroppedDownload(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	s := startDownloadSmoke(t)
	a := s.navigateStarted("pair.bin", "/pair/pair.bin")
	b := s.navigateStarted("pair.bin", "/pair/pair.bin")
	if !strings.EqualFold(a.path, filepath.Join(s.downloads, s.prefix+"pair.bin")) || !strings.EqualFold(b.path, filepath.Join(s.downloads, s.prefix+"pair (1).bin")) {
		t.Fatalf("the two downloads save to %s and %s", a.path, b.path)
	}
	s.settleAmong(b, a.id)
	t.Logf("the dropped download ended %+v; %d requests, ranges %q", b.final, s.requests("pair.bin"), s.rangesAsked("pair.bin"))
	s.expectCompleted(b, "pair (1).bin", droppedSize)
	s.expectBytes(b.path, droppedSize)
	if ranges := s.rangesAsked("pair.bin"); len(ranges) != 3 || ranges[2] != "bytes=65536-" {
		t.Fatalf("the requests asked for %q; the resume this smoke covers did not happen", ranges)
	}

	close(s.pairGo)
	s.settle(a)
	s.expectCompleted(a, "pair.bin", droppedSize)
	s.expectBytes(a.path, droppedSize)
	if matches, _ := filepath.Glob(filepath.Join(s.downloads, s.prefix+"pair*")); len(matches) != 2 {
		t.Fatalf("two clicks left %v", matches)
	}
}

// stallBesideAResumed makes the ambiguous case: download a drops and its
// resume, adopted, runs held at the server; then download b from the same
// address drops. a's operation is its resume's, which reads its reason for
// good, so two read as dropped and b's resume is a row of its own (own),
// saving where WebView2 chose, while b stays running, unreplaced. Both
// resumes run held until twinGo.
func stallBesideAResumed(t *testing.T, s *downloadSmoke) (a, b, own *smokeDownload) {
	t.Helper()
	a = s.navigateStarted("twin.bin", "/twin/twin.bin")
	s.waitRequests("twin.bin", 2)
	b = s.navigateStarted("twin.bin", "/twin/twin.bin")
	s.waitRequests("twin.bin", 4)
	for own == nil {
		r := s.nextReport()
		switch {
		case r.ID == a.id || r.ID == b.id:
			if r.State != DownloadRunning {
				t.Fatalf("a download ended before its resume: %+v", r)
			}
		case r.State == DownloadRunning:
			own = &smokeDownload{id: r.ID, uri: r.URI, path: r.Path, seq: r.Seq}
		default:
			t.Fatalf("waiting for b's resume, the launcher reported %+v", r)
		}
	}
	t.Logf("b's resume is a row of its own saving to %s; ranges %q", own.path, s.rangesAsked("twin.bin"))
	if strings.EqualFold(own.path, a.path) || strings.EqualFold(own.path, b.path) {
		t.Fatalf("b's resume saves to %s, a file of the two", own.path)
	}
	s.ui.do(func() {
		d := s.page().downloads[b.id]
		if d == nil || d.op == nil || len(d.stale) != 0 {
			t.Errorf("b does not read as dropped and unreplaced: %+v", d)
			return
		}
		state, stateErr := d.op.state()
		reason, reasonErr := d.op.interruptReason()
		if stateErr != nil || reasonErr != nil || state != downloadStateInProgress || reason == 0 {
			t.Errorf("b does not read as dropped: state %d (%v), reason %d (%v)", state, stateErr, reason, reasonErr)
		}
	})
	return a, b, own
}

// cancelStalled cancels b, which WebView2 never ends, and asserts the
// launcher ends it canceled once downloadCancelBound passes.
func cancelStalled(t *testing.T, s *downloadSmoke, b *smokeDownload, others ...string) {
	t.Helper()
	canceled := time.Now()
	s.host.Apply(Directive{Op: OpCancelDownload, PageID: "page-1", Download: b.id, Cancel: 1})
	s.settleAmong(b, others...)
	took := time.Since(canceled)
	t.Logf("the stalled download ended after %v: %+v", took, b.final)
	if b.final.State != DownloadInterrupted || b.final.Reason != downloadCancelBoundReason || b.final.End != DownloadEndUntracked || took < downloadCancelBound {
		t.Fatalf("the stalled download ended after %v as %+v", took, b.final)
	}
}

// The ambiguous case left to run: both resumes complete byte for byte, a's
// in its own row and file, and b stays running, saying nothing. The
// backend's cancel of b, which WebView2 ignores, ends it after
// downloadCancelBound; the finished files stay, and nothing more is
// reported.
func TestDownloadSmokeAStalledDownloadsCancelSettles(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	s := startDownloadSmoke(t)
	a, b, own := stallBesideAResumed(t, s)
	close(s.twinGo)
	// The two finish in either order.
	s.settleAll(a, own)
	for _, d := range []*smokeDownload{a, own} {
		if d.final.State != DownloadCompleted {
			t.Fatalf("%s ended %+v", d.id, d.final)
		}
		s.expectBytes(d.path, droppedSize)
	}
	if !strings.EqualFold(a.path, filepath.Join(s.downloads, s.prefix+"twin.bin")) {
		t.Fatalf("a's resume completed at %s, not a's file", a.path)
	}
	if extra := s.quietFor(2 * time.Second); len(extra) != 0 {
		t.Fatalf("b reported %+v", extra)
	}
	cancelStalled(t, s, b)
	for _, d := range []*smokeDownload{a, own} {
		s.expectBytes(d.path, droppedSize)
	}
	if extra := s.quietFor(2 * time.Second); len(extra) != 0 {
		t.Fatalf("after every end, more reports: %+v", extra)
	}
}

// In WebView2 a dropped download and its resume are one download: the
// backend's cancel of b while its resume runs as a row of its own ends
// that resume, canceled, at once, by WebView2's own report; b, which
// WebView2 never ends, ends canceled after downloadCancelBound. a is left
// alone and completes byte for byte.
func TestDownloadSmokeAStalledDownloadsCancelEndsItsResume(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	s := startDownloadSmoke(t)
	a, b, own := stallBesideAResumed(t, s)
	canceled := time.Now()
	s.host.Apply(Directive{Op: OpCancelDownload, PageID: "page-1", Download: b.id, Cancel: 1})
	s.settleAmong(own, a.id, b.id)
	t.Logf("b's cancel ended its resume after %v: %+v", time.Since(canceled), own.final)
	if own.final.State != DownloadInterrupted || own.final.Reason != downloadInterruptReason(downloadUserCanceled) || own.final.End != DownloadEndCanceled {
		t.Fatalf("b's resume ended %+v", own.final)
	}
	s.settleAmong(b, a.id)
	if took := time.Since(canceled); b.final.State != DownloadInterrupted || took < downloadCancelBound {
		t.Fatalf("b ended after %v as %+v", took, b.final)
	}
	close(s.twinGo)
	s.settle(a)
	s.expectCompleted(a, "twin.bin", droppedSize)
	s.expectBytes(a.path, droppedSize)
	s.expectNoFile("twin (")
}

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
			switch report.kind {
			case ReportCreated:
				target = report.detail
			case ReportFindUnavailable:
			case ReportCreateFailed, ReportClosed, ReportProcessFailed, ReportCleared, ReportClearFailed,
				ReportAccelerator, ReportFocused, ReportFocusFailed, ReportDownload, ReportPopup:
				t.Fatalf("create answered %+v", report)
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
	var page *hostPage
	s.ui.do(func() { page = s.host.pages["page-1"] })
	return page
}

// serve answers /cd/<name>?size=n as an attachment, /slow/<name> as one
// that holds until the smoke ends, /fail/<name> as one whose connection
// drops, and /page/<name> as a page with an anchor downloading <name>.
func (s *downloadSmoke) serve(release chan struct{}) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		kind, name, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		s.hitsMu <- struct{}{}
		s.hits[name]++
		s.ranges[name] = append(s.ranges[name], r.Header.Get("Range"))
		<-s.hitsMu
		switch kind {
		case "resume", "norange", "novalidator":
			s.serveDropped(w, r, kind, name)
			return
		case "pair", "twin":
			s.servePair(w, r, kind, name)
			return
		case "page":
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<html><body><a id="a" href="/raw/x" download="%s">a</a></body></html>`, s.prefix+name)
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

// droppedSize is the size of a /resume or /norange download.
const droppedSize = 1 << 20

// droppedByte is byte i of a /resume or /norange download, so a file put
// together from pieces shows whether they line up.
func droppedByte(i int) byte { return byte(i % 251) }

// serveDropped answers the first request for name with its headers and a
// part of its body, then drops the connection, as a server whose response
// is shorter than its Content-Length. A later request gets the rest: for
// "resume", a Range request is answered 206 from its offset; for "norange"
// and "novalidator", every later request gets the whole body with 200, and
// "novalidator" names no Last-Modified to resume against either.
func (s *downloadSmoke) serveDropped(w http.ResponseWriter, r *http.Request, kind, name string) {
	body := droppedHeaders(w, r, kind, s.prefix+name)
	if s.requests(name) == 1 {
		dropAfterPart(w, body)
		return
	}
	_, _ = w.Write(body)
}

// servePair answers the /pair and /twin downloads, each request reaching
// the server (no-store), with ranges. /pair: the first request holds after
// a part of its body until pairGo, the second drops after a part, and a
// later one, its resume, gets the rest. /twin: each odd request (a
// download) drops after a part, and each even one (its resume) sends a
// part, holds until twinGo, then sends the rest.
func (s *downloadSmoke) servePair(w http.ResponseWriter, r *http.Request, kind, name string) {
	w.Header().Set("Cache-Control", "no-store")
	body := droppedHeaders(w, r, "resume", s.prefix+name)
	n := s.requests(name)
	switch {
	case kind == "pair" && n == 1:
		_, _ = w.Write(body[:64<<10])
		w.(http.Flusher).Flush()
		select {
		case <-s.pairGo:
			_, _ = w.Write(body[64<<10:])
		case <-r.Context().Done():
		}
	case kind == "pair" && n == 2:
		dropAfterPart(w, body)
	case kind == "twin" && n%2 == 1:
		dropAfterPart(w, body)
	case kind == "twin":
		_, _ = w.Write(body[:16<<10])
		w.(http.Flusher).Flush()
		select {
		case <-s.twinGo:
			_, _ = w.Write(body[16<<10:])
		case <-r.Context().Done():
		}
	default:
		_, _ = w.Write(body)
	}
}

// dropAfterPart writes a part of body and drops the connection.
func dropAfterPart(w http.ResponseWriter, body []byte) {
	_, _ = w.Write(body[:64<<10])
	w.(http.Flusher).Flush()
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// droppedHeaders writes the headers of a dropped download's response saved
// as file and answers the body to write: for "resume" with validators and
// ranges, a Range request answered from its offset.
func droppedHeaders(w http.ResponseWriter, r *http.Request, kind, file string) []byte {
	ranges := kind == "resume"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	if kind != "novalidator" {
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
	}
	if ranges {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"ao-smoke-1"`)
	}
	from := 0
	if spec, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes="); ok && ranges {
		from, _ = strconv.Atoi(strings.TrimSuffix(spec, "-"))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, droppedSize-1, droppedSize))
		w.Header().Set("Content-Length", strconv.Itoa(droppedSize-from))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(droppedSize))
	}
	body := make([]byte, droppedSize-from)
	for i := range body {
		body[i] = droppedByte(from + i)
	}
	return body
}

// rangesAsked answers the Range header of every request for name, "" for
// none.
func (s *downloadSmoke) rangesAsked(name string) []string {
	s.hitsMu <- struct{}{}
	defer func() { <-s.hitsMu }()
	return append([]string(nil), s.ranges[name]...)
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

func (s *downloadSmoke) permissions(state uint32) {
	s.ui.do(func() { s.perm.answer = state })
}

func (s *downloadSmoke) permissionKinds() []uint32 {
	var kinds []uint32
	s.ui.do(func() { kinds = append(kinds, s.perm.kinds...) })
	return kinds
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

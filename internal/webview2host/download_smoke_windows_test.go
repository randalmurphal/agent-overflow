//go:build windows

package webview2host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The launcher owns a pane page's downloads (download_windows.go). This runs
// that path on the real runtime, on the smoke's own desktop, and
// proves for downloads a page starts:
//
//   - each lands in the user's Downloads folder under its own name, a taken
//     name becoming "name (1).ext", and the started report already names the
//     final file, which keeps WebView2's mark of the web; a 50 MB download
//     reports its progress at most once per downloadProgressInterval;
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
	if limit := int(time.Since(start)/downloadProgressInterval) + 2; big.progress > limit {
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

// Two downloads from one address save to
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
// launcher ends it untracked once downloadCancelBound passes.
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
// WebView2 never ends, ends untracked after downloadCancelBound. a is left
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

package browser

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"
)

// hostedDownloadLog is the Manager's side of the download events.
type hostedDownloadLog struct {
	mu       sync.Mutex
	starts   []downloadStart
	progress []downloadProgress
	refuse   bool
	// admitting, when set, is called as the Manager admits a download,
	// before it answers: the admission hook.
	admitting func(downloadStart)
	// progressing, when set, is called as the Manager takes a download's
	// progress, before it is recorded.
	progressing func(downloadProgress)
}

func (r *hostedDownloadLog) events() engineEvents {
	return engineEvents{
		DownloadStarted: func(start downloadStart) downloadAdmission {
			r.mu.Lock()
			hook := r.admitting
			r.mu.Unlock()
			if hook != nil {
				hook(start)
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			r.starts = append(r.starts, start)
			if r.refuse {
				return downloadRefused
			}
			return downloadAdmitted
		},
		DownloadProgress: func(progress downloadProgress) {
			r.mu.Lock()
			hook := r.progressing
			r.mu.Unlock()
			if hook != nil {
				hook(progress)
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			r.progress = append(r.progress, progress)
		},
	}
}

func (r *hostedDownloadLog) take() ([]downloadStart, []downloadProgress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	starts, progress := r.starts, r.progress
	r.starts, r.progress = nil, nil
	return starts, progress
}

// testBackendPath stands in for `wslpath -u` of a drive path.
func testBackendPath(_ context.Context, path string) (string, error) {
	drive, rest, ok := strings.Cut(path, `:\`)
	if !ok || len(drive) != 1 {
		return "", fmt.Errorf("wslpath: %s: not a drive path", path)
	}
	return "/mnt/" + strings.ToLower(drive) + "/" + strings.ReplaceAll(rest, `\`, "/"), nil
}

func newDownloadHostedEngine(t *testing.T) (*hostedEngine, *directiveSink, *hostedDownloadLog) {
	t.Helper()
	recorder := &hostedDownloadLog{}
	engine, sink := newTestHostedEngine(t, stubRelay{}, recorder.events())
	engine.backendPath = testBackendPath
	// No status question unless a test asks for one.
	engine.downloadQuiet = time.Hour
	t.Cleanup(engine.Stop)
	return engine, sink, recorder
}

// testSnapshotNumbers numbers the snapshots a test sends without one, per
// engine and download, as the launcher numbers them. The map holds each
// engine, so no later test's engine takes its address and continues its
// numbers.
var testSnapshotNumbers = struct {
	sync.Mutex
	last map[*hostedEngine]map[string]uint64
}{last: map[*hostedEngine]map[string]uint64{}}

// settleReports waits until the engine has processed every report of
// pageID it accepted, or of every page when pageID is empty.
func settleReports(t *testing.T, engine *hostedEngine, pageID string) {
	t.Helper()
	if !reportsProcessed(engine, pageID) {
		t.Fatalf("the reports of page %q were not processed", pageID)
	}
}

// reportsProcessed answers whether the engine processed every report of
// pageID (of every page when empty) it accepted, within hostedTestDeadline.
func reportsProcessed(engine *hostedEngine, pageID string) bool {
	for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
		busy := engine.pages.busy()
		if pageID != "" {
			_, busy = engine.pages.laneOf(ids.PageHandle(pageID))
		}
		if !busy {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// reportDownload has the launcher send one snapshot of a page's download,
// numbered after the download's last one unless it carries its number, and
// waits until the engine processed it.
func reportDownload(t *testing.T, engine *hostedEngine, pageID string, report webview2host.DownloadReport) {
	t.Helper()
	sendDownload(t, engine, pageID, report)
	settleReports(t, engine, pageID)
}

// sendDownload is reportDownload without the wait: it returns once the
// engine acknowledged the snapshot.
func sendDownload(t *testing.T, engine *hostedEngine, pageID string, report webview2host.DownloadReport) {
	t.Helper()
	if report.Seq == 0 && report.State != webview2host.DownloadUnknown {
		testSnapshotNumbers.Lock()
		numbers := testSnapshotNumbers.last[engine]
		if numbers == nil {
			numbers = map[string]uint64{}
			testSnapshotNumbers.last[engine] = numbers
		}
		numbers[report.ID]++
		report.Seq = numbers[report.ID]
		testSnapshotNumbers.Unlock()
	}
	detail, err := report.Encode()
	if err != nil {
		t.Fatal(err)
	}
	engine.Report(ids.PageHandle(pageID), webview2host.ReportDownload, detail)
}

// isCancelOf reports whether directive is the launcher's cancel of download
// id in page pageID.
func isCancelOf(directive webview2host.Directive, pageID, id string) bool {
	return directive.Op == webview2host.OpCancelDownload && directive.PageID == pageID && directive.Download == id
}

const testDownloadPath = `C:\Users\dev\Downloads\report (1).pdf`

// snapshot is a download's snapshot at testDownloadPath.
func snapshot(id, state string, received int64) webview2host.DownloadReport {
	report := webview2host.DownloadReport{ID: id, State: state, URI: "https://example.test/report.pdf", Name: "report (1).pdf", Path: testDownloadPath, Received: received, Total: 100}
	if state == webview2host.DownloadInterrupted {
		report.End, report.Reason = webview2host.DownloadEndCanceled, "the download was canceled"
	}
	return report
}

// terminals counts the terminal events among progress.
func terminals(progress []downloadProgress) int {
	n := 0
	for _, p := range progress {
		if p.State != downloadInProgress {
			n++
		}
	}
	return n
}

// unknownAnswer is the launcher's answer for an id it has no record of.
func unknownAnswer(id string) webview2host.DownloadReport {
	return webview2host.DownloadReport{ID: id, State: webview2host.DownloadUnknown, Reason: webview2host.DownloadUnknownReason, Answer: true}
}

// heldAdmission makes the Manager's next admission wait: entered closes as
// it begins, and the admission answers once release is closed.
func heldAdmission(recorder *hostedDownloadLog) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	recorder.mu.Lock()
	recorder.admitting = func(downloadStart) {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	recorder.mu.Unlock()
	return entered, release
}

// reportAsync sends a snapshot from a goroutine of its own, as the
// launcher's reports arrive, and answers a channel closed once the page's
// accepted reports are applied.
func reportAsync(t *testing.T, engine *hostedEngine, pageID string, report webview2host.DownloadReport) chan struct{} {
	t.Helper()
	detail, err := report.Encode()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.Report(ids.PageHandle(pageID), webview2host.ReportDownload, detail)
		if !reportsProcessed(engine, pageID) {
			t.Errorf("the reports of page %s were not processed", pageID)
		}
	}()
	return done
}

// awaitQueued waits until a snapshot of download id with received bytes
// waits on page's lane: received, and not applied yet.
func awaitQueued(t *testing.T, engine *hostedEngine, pageID, id string, received int64) {
	t.Helper()
	suffix := fmt.Sprintf(" %s %d", id, received)
	for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
		if slices.ContainsFunc(engine.pages.waiting(ids.PageHandle(pageID)), func(w string) bool { return strings.HasSuffix(w, suffix) }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no snapshot of %s at %d bytes waits on page %s's lane: %v", id, received, pageID, engine.pages.waiting(ids.PageHandle(pageID)))
		}
	}
}

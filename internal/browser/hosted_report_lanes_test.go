package browser

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"
)

// Through the launcher's own report queue: page-b's
// download completes and its path conversion blocks; page-a's Ctrl-click
// popup, reported after it, is recorded with its opener, and its workspace
// file request passes page-a's policy, while the conversion still blocks.
// Page-b's progress meanwhile waits as one snapshot, its close behind it,
// and its completion reaches the Manager before its close.
func TestASlowCompletionHoldsNoOtherPagesReport(t *testing.T) {
	engine, _ := newPolicyEngine(t)
	engine.ownerWait = 500 * time.Millisecond
	var mu sync.Mutex
	var seen []string
	note := func(format string, args ...any) {
		mu.Lock()
		seen = append(seen, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	took := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
	awaitSeen := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(hostedTestDeadline); !slices.Contains(took(), want); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the Manager never heard %q: %v", want, took())
			}
		}
	}
	engine.events.DownloadStarted = func(start downloadStart) downloadAdmission {
		note("start %s", start.ID)
		return downloadAdmitted
	}
	engine.events.DownloadProgress = func(p downloadProgress) { note("%s %s %v", p.ID, p.State, p.Received) }
	engine.events.PageClosed = func(handle ids.PageHandle) { note("closed %s", handle) }
	entered, release := make(chan struct{}), make(chan struct{})
	engine.backendPath = func(ctx context.Context, path string) (string, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return testBackendPath(ctx, path)
	}
	queue := webview2host.NewReportQueue(func(pageID string, kind webview2host.ReportKind, detail string) {
		engine.Report(ids.PageHandle(pageID), kind, detail)
	})
	encode := func(report webview2host.DownloadReport) string {
		t.Helper()
		detail, err := report.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return detail
	}
	sendSnapshot := func(report webview2host.DownloadReport) {
		queue.Submit("page-b", webview2host.ReportDownload, encode(report))
	}
	numbered := func(report webview2host.DownloadReport, seq uint64) webview2host.DownloadReport {
		report.Seq = seq
		return report
	}

	sendSnapshot(numbered(snapshot("dl-1", webview2host.DownloadRunning, 10), 1))
	sendSnapshot(numbered(snapshot("dl-2", webview2host.DownloadRunning, 1), 1))
	awaitSeen("dl-2 inProgress 1")
	sendSnapshot(numbered(snapshot("dl-1", webview2host.DownloadCompleted, 100), 2))
	awaitClosed(t, entered, "the completion's path conversion")
	// Each acknowledged at once, so the launcher's queue replaces none of
	// them.
	for i := range 49 {
		engine.Report("page-b", webview2host.ReportDownload, encode(numbered(snapshot("dl-2", webview2host.DownloadRunning, int64(i+2)), uint64(i+2))))
	}
	// A Ctrl-click: the popup's target names no opener; the launcher's
	// report does.
	engine.currentTargets().created(&target.Info{TargetID: "T-pop", Type: "page"}, nil)
	queue.Submit("popup-1", webview2host.ReportPopup, `{"opener":"page-a","target":"T-pop"}`)
	queue.Submit("page-b", webview2host.ReportClosed, "")

	if !awaitRequestOwner(t.Context(), engine.ownerWait, func() (bool, bool) {
		return engine.allowRequest(engine.currentTargets(), "T-pop", "file:///w/one.html")
	}, t.Logf) {
		t.Fatal("the popup's workspace file was refused while another page's completion converted its path")
	}
	engine.mu.Lock()
	popup := engine.popups["popup-1"]
	engine.mu.Unlock()
	if popup == nil || popup.opener != "page-a" {
		t.Fatalf("the popup was recorded as %+v, want opened by page-a", popup)
	}
	want := []string{fmt.Sprintf("%d dl-2 50", acceptedSnapshot), fmt.Sprintf("%d  0", acceptedClose)}
	var waiting []string
	for deadline := time.Now().Add(hostedTestDeadline); !slices.Equal(waiting, want); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("page-b's lane waits on %v, want its progress as one snapshot, then its close", waiting)
		}
		waiting = engine.pages.waiting("page-b")
	}
	if got := took(); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "dl-1 completed") || s == "closed page-b" }) {
		t.Fatalf("page-b's completion or close reached the Manager before its path was converted: %v", got)
	}

	close(release)
	awaitSeen("closed page-b")
	settleReports(t, engine, "")
	got := took()
	completed := slices.IndexFunc(got, func(s string) bool { return strings.HasPrefix(s, "dl-1 completed") })
	closed := slices.Index(got, "closed page-b")
	if completed < 0 || completed > closed {
		t.Fatalf("the Manager heard %v, want page-b's completion before its close", got)
	}
	var progress []string
	for _, s := range got {
		if strings.HasPrefix(s, "dl-2 ") {
			progress = append(progress, s)
		}
	}
	if !slices.Equal(progress, []string{"dl-2 inProgress 1", "dl-2 inProgress 50", "dl-2 pageGone 0"}) {
		t.Fatalf("dl-2's progress reached the Manager as %v, want its first, its newest, and its end with its page", progress)
	}
	if ended := slices.Index(got, "dl-2 pageGone 0"); ended > closed {
		t.Fatalf("the Manager heard %v, want page-b's running download ended before its close", got)
	}
}

// A popup not listed yet runs its work on its opener's lane: its download's
// end, still converting its path, reaches the Manager before the close of
// the opener the launcher reported after it, and before the popup's own
// close or crash once the popup is listed meanwhile.
func TestAPendingPopupsEndPrecedesItsOpenersClose(t *testing.T) {
	for _, tc := range []struct {
		page string
		kind webview2host.ReportKind
	}{
		{"opener", webview2host.ReportClosed},
		{"popup", webview2host.ReportClosed},
		{"popup", webview2host.ReportProcessFailed},
	} {
		t.Run(fmt.Sprintf("%s %s", tc.page, tc.kind), func(t *testing.T) { testAPendingPopupsEndPrecedesAClose(t, tc.page, tc.kind) })
	}
}

func testAPendingPopupsEndPrecedesAClose(t *testing.T, closes string, kind webview2host.ReportKind) {
	engine, _, recorder := newDownloadHostedEngine(t)
	var mu sync.Mutex
	var order []string
	engine.events.PageClosed = func(handle ids.PageHandle) {
		mu.Lock()
		order = append(order, "closed "+string(handle))
		mu.Unlock()
	}
	engine.events.PageCrashed = func(handle ids.PageHandle, _ string) {
		mu.Lock()
		order = append(order, "closed "+string(handle))
		mu.Unlock()
	}
	recorder.mu.Lock()
	recorder.progressing = func(p downloadProgress) {
		if p.State != downloadInProgress {
			mu.Lock()
			order = append(order, fmt.Sprintf("%s %s", p.ID, p.State))
			mu.Unlock()
		}
	}
	recorder.mu.Unlock()
	bindTestPage(t, engine, "opener", "T-opener")
	pendingTestPopup(engine, "popup", "T-popup", "opener")
	reportDownload(t, engine, "popup", snapshot("dl-1", webview2host.DownloadRunning, 10))
	entered, release := make(chan struct{}), make(chan struct{})
	engine.backendPath = func(ctx context.Context, path string) (string, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return testBackendPath(ctx, path)
	}
	sendDownload(t, engine, "popup", snapshot("dl-1", webview2host.DownloadCompleted, 100))
	awaitClosed(t, entered, "the completion's path conversion")
	if closes == "popup" {
		// Its attach binds it.
		bindTestPage(t, engine, "popup", "T-popup")
	}
	engine.Report(ids.PageHandle(closes), kind, "render-process-exited")
	time.Sleep(20 * time.Millisecond)
	close(release)
	settleReports(t, engine, "")
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(order, []string{"dl-1 completed", "closed " + closes}) {
		t.Fatalf("the Manager heard %v, want the popup's download end before the close of the %s", order, closes)
	}
}

// Stop drops the work still waiting, ends a path conversion under way, and
// returns once the lane's goroutine has: a received end, the one whose
// conversion it ended, reaches the Manager as received, before Stop
// returns, a running snapshot not at all, and nothing after it.
func TestStopJoinsTheAcceptedReports(t *testing.T) {
	engine, _, recorder := newDownloadHostedEngine(t)
	bindTestPage(t, engine, "page1", "TARGET-1")
	reportDownload(t, engine, "page1", snapshot("dl-1", webview2host.DownloadRunning, 10))
	reportDownload(t, engine, "page1", snapshot("dl-2", webview2host.DownloadRunning, 10))
	entered, left := make(chan struct{}), make(chan struct{})
	engine.backendPath = func(ctx context.Context, _ string) (string, error) {
		close(entered)
		defer close(left)
		<-ctx.Done()
		return "", ctx.Err()
	}
	recorder.take()
	sendDownload(t, engine, "page1", snapshot("dl-1", webview2host.DownloadCompleted, 100))
	awaitClosed(t, entered, "the completion's path conversion")
	sendDownload(t, engine, "page1", snapshot("dl-2", webview2host.DownloadRunning, 20))
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		engine.Stop()
	}()
	awaitClosed(t, stopped, "Stop")
	select {
	case <-left:
	default:
		t.Fatal("Stop returned with the path conversion still under way")
	}
	_, progress := recorder.take()
	time.Sleep(20 * time.Millisecond)
	if _, late := recorder.take(); len(progress) != 1 || progress[0].ID != "dl-1" || progress[0].State != downloadCompleted || progress[0].FilePath != "" || len(late) != 0 {
		t.Fatalf("the Manager heard %+v while the engine stopped and %+v after", progress, late)
	}
}

// A page's end the engine learns of by itself (its renderer's crash or its
// connection's loss read from CDP, the close the launcher never reported)
// runs on the page's lane like the launcher's own reports: a completion
// still converting its path, and one accepted behind it, reach the Manager
// first.
func TestAPagesOwnEndFollowsItsAcceptedReports(t *testing.T) {
	for name, end := range map[string]func(*hostedEngine){
		"its renderer crashed": func(engine *hostedEngine) {
			engine.targetCrashed(target.EventTargetCrashed{TargetID: "T-1"})
		},
		"its connection was lost": func(engine *hostedEngine) { engine.reportLost([]ids.PageHandle{"page1"}) },
		"the launcher never reported its close": func(engine *hostedEngine) {
			engine.closeFallback = time.Millisecond
			engine.awaitLauncherClose("page1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			engine, _, recorder := newDownloadHostedEngine(t)
			var mu sync.Mutex
			var order []string
			note := func(what string) {
				mu.Lock()
				order = append(order, what)
				mu.Unlock()
			}
			engine.events.PageClosed = func(handle ids.PageHandle) { note("ended " + string(handle)) }
			engine.events.PageCrashed = func(handle ids.PageHandle, _ string) { note("ended " + string(handle)) }
			recorder.mu.Lock()
			recorder.progressing = func(p downloadProgress) {
				if p.State != downloadInProgress {
					note(fmt.Sprintf("%s %s", p.ID, p.State))
				}
			}
			recorder.mu.Unlock()
			bindTestPage(t, engine, "page1", "T-1")
			reportDownload(t, engine, "page1", snapshot("dl-1", webview2host.DownloadRunning, 10))
			reportDownload(t, engine, "page1", snapshot("dl-2", webview2host.DownloadRunning, 10))
			entered, release := make(chan struct{}), make(chan struct{})
			var first sync.Once
			engine.backendPath = func(ctx context.Context, path string) (string, error) {
				first.Do(func() { close(entered) })
				select {
				case <-release:
				case <-ctx.Done():
					return "", ctx.Err()
				}
				return testBackendPath(ctx, path)
			}
			sendDownload(t, engine, "page1", snapshot("dl-1", webview2host.DownloadCompleted, 100))
			awaitClosed(t, entered, "the completion's path conversion")
			sendDownload(t, engine, "page1", snapshot("dl-2", webview2host.DownloadCompleted, 100))
			end(engine)
			time.Sleep(20 * time.Millisecond)
			close(release)
			settleReports(t, engine, "")
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(order, []string{"dl-1 completed", "dl-2 completed", "ended page1"}) {
				t.Fatalf("the Manager heard %v, want both downloads' completions before the page's end", order)
			}
		})
	}
}

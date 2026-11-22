//go:build windows

package webview2host

import (
	"context"
	"testing"
	"time"
)

func TestAwaitReportLeavesFollowingReportsUnread(t *testing.T) {
	r := newPopupRig(t)
	r.host.config.Report("page", ReportClosed, "")
	r.host.config.Report("page", ReportClosed, "")
	sameReports(t, "await", kinds(t, r.awaitReport("page", ReportClosed)), "page:closed")
	sameReports(t, "reports remaining after await", kinds(t, r.takeReports()), "page:closed")
	r.done()
}

func TestSmokeWindowStepChecksEarlierObservations(t *testing.T) {
	// This unexpected window is still visible when the next step starts.
	window := &smokeWindowSeen{hwnd: 2, class: "unexpected", shown: time.Now()}
	watch := &smokeWindowWatch{own: 1, current: map[uintptr]*smokeWindowSeen{2: window}, seen: []*smokeWindowSeen{window}}
	ran := false
	if err := watch.runStep(t, func() { ran = true }, nil); err == nil || ran {
		t.Fatalf("step ignored an earlier window observation, ran=%v, error=%v", ran, err)
	}
}

func TestSmokeWindowWatchChecksFinalObservations(t *testing.T) {
	watch := &smokeWindowWatch{own: 1, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(watch.done)
		<-watch.stop
		watch.seen = append(watch.seen, &smokeWindowSeen{hwnd: 2, class: "unexpected", shown: time.Now()})
	}()
	if err := watch.finish(t); err == nil {
		t.Fatal("window watch ignored its final observation")
	}
}

func TestPolicyReportReaderJoinsWithReportsChannelStillOpen(t *testing.T) {
	reports := make(chan smokeReport, 1)
	defer close(reports)
	var done <-chan struct{}
	t.Run("reader lifetime", func(t *testing.T) {
		received := make(chan string, 1)
		done = readPolicyReports(t, reports, func(target string) { received <- target })
		reports <- smokeReport{kind: ReportPopup, detail: `{"opener":"page-1","target":"popup-target"}`}
		select {
		case got := <-received:
			if got != "popup-target" {
				t.Errorf("reported %q", got)
			}
		case <-time.After(time.Second):
			t.Error("the reader never delivered the popup")
		}
	})
	select {
	case <-done:
	default:
		t.Fatal("the report reader survived test cleanup")
	}
}

func TestPolicyReportCleanupWaitsForInFlightCallback(t *testing.T) {
	reports := make(chan smokeReport, 1)
	defer close(reports)
	ctx, release := context.WithCancel(t.Context())
	defer release()
	entered := make(chan struct{})
	var done <-chan struct{}
	t.Run("callback in flight", func(t *testing.T) {
		done = readPolicyReports(t, reports, func(string) {
			close(entered)
			<-ctx.Done()
		})
		reports <- smokeReport{kind: ReportPopup, detail: `{"opener":"page-1","target":"popup-target"}`}
		select {
		case <-entered:
		case <-time.After(time.Second):
			release()
			t.Fatal("the popup callback never started")
		}
		// Give cleanup a chance to return early if it does not join.
		time.AfterFunc(250*time.Millisecond, release)
	})
	select {
	case <-done:
	default:
		t.Error("cleanup returned while the popup callback was still running")
	}
}

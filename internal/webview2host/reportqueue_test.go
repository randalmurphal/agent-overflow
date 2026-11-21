package webview2host

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// While delivery stalls, a download's waiting progress is replaced in its
// place by its newer progress, so the reports behind it wait for one
// snapshot, not one per interval. Answers, terminal snapshots and every
// other kind are delivered as submitted, in order.
func TestReportQueueCoalescesWaitingProgressInPlace(t *testing.T) {
	stall := make(chan struct{})
	delivered := make(chan string, 64)
	q := NewReportQueue(func(pageID string, kind ReportKind, detail string) {
		if kind == ReportCreated && pageID == "stall" {
			<-stall
		}
		delivered <- fmt.Sprintf("%s %s %s", pageID, kind, detail)
	})
	snapshot := func(id string, seq uint64, state string, answer bool) string {
		detail, err := DownloadReport{ID: id, Seq: seq, State: state, Answer: answer, Received: int64(seq)}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return detail
	}
	q.Submit("stall", ReportCreated, "")
	q.Submit("p", ReportDownload, snapshot("d1", 1, DownloadRunning, false))
	q.Submit("p", ReportDownload, snapshot("d2", 1, DownloadRunning, false))
	for seq := uint64(2); seq <= 20; seq++ {
		q.Submit("p", ReportDownload, snapshot("d1", seq, DownloadRunning, false))
	}
	q.Submit("p", ReportPopup, "popup")
	q.Submit("p", ReportDownload, snapshot("d1", 21, DownloadRunning, true))
	q.Submit("p", ReportDownload, snapshot("d2", 2, DownloadRunning, false))
	q.Submit("p", ReportDownload, snapshot("d1", 22, DownloadCompleted, false))
	q.Submit("p", ReportClosed, "")
	close(stall)

	want := []string{
		"stall created ",
		"p download " + snapshot("d1", 20, DownloadRunning, false),
		"p download " + snapshot("d2", 2, DownloadRunning, false),
		"p popup popup",
		"p download " + snapshot("d1", 21, DownloadRunning, true),
		"p download " + snapshot("d1", 22, DownloadCompleted, false),
		"p closed ",
	}
	var got []string
	for range want {
		select {
		case report := <-delivered:
			got = append(got, report)
		case <-time.After(5 * time.Second):
			t.Fatalf("delivered %v, then stalled", got)
		}
	}
	select {
	case extra := <-delivered:
		t.Fatalf("delivered %v, then %q", got, extra)
	case <-time.After(50 * time.Millisecond):
	}
	if !slices.Equal(got, want) {
		t.Fatalf("delivered\n%v\nwant\n%v", got, want)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.progress) != 0 || len(q.jobs) != 0 {
		t.Fatalf("drained queue holds %d jobs and %d progress entries", len(q.jobs), len(q.progress))
	}
}

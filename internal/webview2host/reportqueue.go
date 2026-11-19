package webview2host

import (
	"encoding/json"
	"sync"
)

// ReportQueue delivers a host's reports one at a time, in the order they
// were submitted, on a goroutine that exists only while reports wait.
// Submission never blocks: the host reports from WebView2 completion
// handlers on the UI thread.
//
// Delivery can stall for a whole RPC timeout while a download keeps
// reporting its bytes, so a running download snapshot that is still waiting
// is replaced in its place by the same download's next one rather than
// queued behind it. The backend applies only a download's newest snapshot
// (Seq), so the replaced one carries nothing the newer one does not, and
// taking the older one's place keeps it ahead of every report submitted
// after that one: the page's and popup's reports, other downloads', and its
// own terminal snapshot. Answers and terminal snapshots are never replaced
// and never replace.
type ReportQueue struct {
	deliver func(pageID string, kind ReportKind, detail string)

	mu   sync.Mutex
	jobs []*queuedReport
	// progress is each waiting running snapshot that a newer one replaces,
	// by page and download id.
	progress map[progressKey]*queuedReport
	busy     bool
}

type queuedReport struct {
	pageID string
	kind   ReportKind
	detail string
	key    progressKey
}

type progressKey struct{ pageID, downloadID string }

// NewReportQueue returns a queue that hands each report to deliver.
func NewReportQueue(deliver func(pageID string, kind ReportKind, detail string)) *ReportQueue {
	return &ReportQueue{deliver: deliver, progress: make(map[progressKey]*queuedReport)}
}

// Submit queues one report and starts delivery if none is running.
func (q *ReportQueue) Submit(pageID string, kind ReportKind, detail string) {
	key, coalesce := progressKeyOf(pageID, kind, detail)
	q.mu.Lock()
	if coalesce {
		if waiting := q.progress[key]; waiting != nil {
			waiting.detail = detail
			q.mu.Unlock()
			return
		}
	}
	job := &queuedReport{pageID: pageID, kind: kind, detail: detail}
	if coalesce {
		job.key = key
		q.progress[key] = job
	}
	q.jobs = append(q.jobs, job)
	if q.busy {
		q.mu.Unlock()
		return
	}
	q.busy = true
	q.mu.Unlock()
	go q.run()
}

func (q *ReportQueue) run() {
	for {
		q.mu.Lock()
		if len(q.jobs) == 0 {
			q.busy = false
			q.mu.Unlock()
			return
		}
		job := q.jobs[0]
		q.jobs[0] = nil
		q.jobs = q.jobs[1:]
		if q.progress[job.key] == job {
			delete(q.progress, job.key)
		}
		pageID, kind, detail := job.pageID, job.kind, job.detail
		q.mu.Unlock()
		q.deliver(pageID, kind, detail)
	}
}

// progressKeyOf names the download a report is a running, unanswered
// snapshot of; any other report is never replaced.
func progressKeyOf(pageID string, kind ReportKind, detail string) (progressKey, bool) {
	if kind != ReportDownload {
		return progressKey{}, false
	}
	var snapshot struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		Answer bool   `json:"answer"`
	}
	if err := json.Unmarshal([]byte(detail), &snapshot); err != nil ||
		snapshot.ID == "" || snapshot.State != DownloadRunning || snapshot.Answer {
		return progressKey{}, false
	}
	return progressKey{pageID: pageID, downloadID: snapshot.ID}, true
}

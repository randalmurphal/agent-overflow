package browser

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
)

// pageDownloadLog is one page's download log for browser_downloads: every
// download the page admitted, numbered in admission order (Sequence, which
// After waits past), with every running download and the newest
// maxPageDownloads settled ones by settle order. Its fields are written
// only by its own methods: a change is made in a hold (hold), whose
// release numbers what settled, drops the earliest settled past the
// bound and wakes the waiters, so no writer can leave the log over its
// bound or a waiter asleep. A running download is never dropped: its
// progress, its reservation and its cancel all need it. Only artifact
// downloads (downloadsAsArtifacts) have quotas, which bound how many of
// them run.
type pageDownloadLog struct {
	mu      sync.Mutex
	entries []DownloadInfo
	// seq numbers the entries as they are admitted; settles numbers them
	// as they settle (DownloadInfo.settled).
	seq, settles uint64
	// wait is closed and replaced at each release that added or settled an
	// entry; nil until a waiter needs one.
	wait chan struct{}
	// ended is set when the page stops being the Manager's (pageLeftLocked):
	// it admits no download after.
	ended bool
}

// downloadLogHold is a change to a page's download log in progress, the
// log locked from hold to release. An entry pointer it answers is valid
// until release; release ends the hold, and any use after it panics.
type downloadLogHold struct {
	log *pageDownloadLog
	// added is set when the hold added an entry.
	added bool
}

// hold locks the log for a change.
func (l *pageDownloadLog) hold() *downloadLogHold {
	l.mu.Lock()
	return &downloadLogHold{log: l}
}

// release numbers the entries that settled in the hold, drops the earliest
// settled past maxPageDownloads, wakes the waiters when an entry was added
// or settled, and unlocks the log.
func (h *downloadLogHold) release() {
	l := h.log
	h.log = nil
	settledNow := false
	settled := 0
	for i := range l.entries {
		entry := &l.entries[i]
		if entry.State == downloadStateRunning {
			continue
		}
		if entry.settled == 0 {
			l.settles++
			entry.settled = l.settles
			settledNow = true
		}
		settled++
	}
	if settled > maxPageDownloads {
		stamps := make([]uint64, 0, settled)
		for _, entry := range l.entries {
			if entry.settled != 0 {
				stamps = append(stamps, entry.settled)
			}
		}
		slices.Sort(stamps)
		cutoff := stamps[settled-maxPageDownloads-1]
		l.entries = slices.DeleteFunc(l.entries, func(entry DownloadInfo) bool {
			return entry.settled != 0 && entry.settled <= cutoff
		})
	}
	if (h.added || settledNow) && l.wait != nil {
		close(l.wait)
		l.wait = nil
	}
	l.mu.Unlock()
}

// ended is whether the page stopped admitting downloads.
func (h *downloadLogHold) ended() bool { return h.log.ended }

// end stops the page admitting downloads, for good.
func (h *downloadLogHold) end() { h.log.ended = true }

// add numbers entry as the page's newest download and answers it in the
// log. The caller checked ended.
func (h *downloadLogHold) add(entry DownloadInfo) *DownloadInfo {
	l := h.log
	l.seq++
	entry.Sequence = l.seq
	l.entries = append(l.entries, entry)
	h.added = true
	return &l.entries[len(l.entries)-1]
}

// entry answers the log's entry for an engine download id, nil for none.
func (h *downloadLogHold) entry(id downloadID) *DownloadInfo {
	l := h.log
	for i := len(l.entries) - 1; i >= 0; i-- {
		if downloadID(l.entries[i].ID) == id {
			return &l.entries[i]
		}
	}
	return nil
}

// publishing answers the entry of download id whose file is being
// published (rowPublishing), which stays in the log until its publish
// settles it: a running entry is never trimmed, and neither a report, a
// cancel nor its page's departure settles one being published. The error
// says that broke.
func (h *downloadLogHold) publishing(id downloadID) (*DownloadInfo, error) {
	entry := h.entry(id)
	if entry == nil {
		return nil, fmt.Errorf("%w: download %s left its page's log while its file was published", errDownloadPhase, id)
	}
	if entry.phase != rowPublishing {
		return nil, fmt.Errorf("%w: download %s is %v while its file was published", errDownloadPhase, id, entry.phase)
	}
	return entry, nil
}

// running answers the entries still running.
func (h *downloadLogHold) running() []*DownloadInfo {
	var out []*DownloadInfo
	for i := range h.log.entries {
		if h.log.entries[i].State == downloadStateRunning {
			out = append(out, &h.log.entries[i])
		}
	}
	return out
}

// list answers a copy of the log, oldest first.
func (l *pageDownloadLog) list() []DownloadInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries)
}

// lastSequence is the Sequence of the newest entry, zero for none.
func (l *pageDownloadLog) lastSequence() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// has is whether the log has an entry for an engine download id.
func (l *pageDownloadLog) has(id downloadID) bool {
	h := l.hold()
	defer h.release()
	return h.entry(id) != nil
}

// completedFiles answers the files of the completed entries at path, as
// they were when each completed.
func (l *pageDownloadLog) completedFiles(path string) []os.FileInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	var files []os.FileInfo
	for _, entry := range l.entries {
		if entry.State == downloadStateCompleted && entry.Path == path && entry.file != nil {
			files = append(files, entry.file)
		}
	}
	return files
}

// settledAfter waits for the first entry past after that has settled.
func (l *pageDownloadLog) settledAfter(ctx context.Context, after uint64) (DownloadInfo, error) {
	for {
		l.mu.Lock()
		for _, entry := range l.entries {
			if entry.Sequence > after && entry.State != downloadStateRunning {
				l.mu.Unlock()
				return entry, nil
			}
		}
		if l.wait == nil {
			l.wait = make(chan struct{})
		}
		signal := l.wait
		l.mu.Unlock()
		select {
		case <-signal:
		case <-ctx.Done():
			return DownloadInfo{}, fmt.Errorf("browser: wait for download: %w", ctx.Err())
		}
	}
}

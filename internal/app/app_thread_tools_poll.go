package app

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/store"
)

// The unattended half of the request ledger: what runs with no tool call in
// flight. One ticker fires due reminders, ages out collected answers and
// deletes rows past the retention floor; one boot sweep settles what a
// restart interrupted.
//
// It is deliberately one loop rather than three. Each pass is two indexed
// queries against tables that are empty on most computers, and a single
// goroutine is one thing to cancel and join at shutdown.

const (
	// threadRequestSweepInterval is the reminder clock's resolution. A
	// reminder is a message to the person's own agent, so a second of slack
	// is invisible and the query behind it is an index seek on an empty
	// table.
	threadRequestSweepInterval = time.Second
	// threadRequestExpiryInterval is how often the retention work runs. It
	// writes, so it runs on its own much slower clock.
	threadRequestExpiryInterval = 10 * time.Minute
	// threadRequestPollBatch bounds one pass over the remote rows.
	threadRequestPollBatch = 32
)

// startThreadRequestSweeps arms the ticker. It is called once, from the
// unattended-work startup phase, beside the remote watches it is modelled on.
func (a *App) startThreadRequestSweeps() {
	a.threadRequests.mu.Lock()
	if a.threadRequests.nudge != nil {
		a.threadRequests.mu.Unlock()
		return
	}
	nudge := make(chan struct{}, 1)
	a.threadRequests.nudge = nudge
	a.threadRequests.mu.Unlock()

	a.threadRequestsWG.Add(1)
	go func() {
		defer a.threadRequestsWG.Done()
		a.runThreadRequestSweeps(nudge)
	}()
}

// nudgeThreadRequestSweep asks for a pass now. A reminder due sooner than the
// next tick would otherwise wait it out. Non-blocking: a pass already pending
// covers this one.
func (a *App) nudgeThreadRequestSweep() {
	a.threadRequests.mu.Lock()
	nudge := a.threadRequests.nudge
	a.threadRequests.mu.Unlock()
	if nudge == nil {
		return
	}
	select {
	case nudge <- struct{}{}:
	default:
	}
}

func (a *App) runThreadRequestSweeps(nudge <-chan struct{}) {
	ticker := time.NewTicker(threadRequestSweepInterval)
	defer ticker.Stop()
	done := a.lifeCtx().Done()
	lastExpiry := time.Now()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		case <-nudge:
		}
		if a.shuttingDown.Load() {
			return
		}
		now := time.Now()
		a.fireDueThreadReminders(now)
		a.pollRemoteThreadRequests(now)
		if now.Sub(lastExpiry) >= threadRequestExpiryInterval {
			lastExpiry = now
			a.expireThreadRequests(now)
		}
	}
}

// pollRemoteThreadRequests is the seam a paired computer's requests are
// collected through: the due-row batch is already indexed for it, and the
// cadence table lives in the same sweep as the remote watches'.
//
// This build accepts no request with a target computer, so a due row here is
// a defect rather than work: reporting it is the honest answer until the peer
// half exists, and a stub RPC would be a second implementation to keep true.
func (a *App) pollRemoteThreadRequests(now time.Time) {
	rows, err := a.store.DueThreadRequestPolls(now.UnixMilli(), threadRequestPollBatch)
	if err != nil {
		logThreadRequestSweep("read due request polls", err)
		return
	}
	for _, row := range rows {
		log.Printf("thread tools: request %s names computer %s, which this build cannot reach; it stays open", row.Token, row.TargetComputerID)
		// Back off hard: the row cannot progress, and the log line is the
		// only thing a pass over it produces.
		if err := a.store.RescheduleThreadRequest(row.Token, now.Add(time.Hour).UnixMilli(), row.Attempts+1,
			"This computer cannot reach the computer that request went to."); err != nil {
			logThreadRequestSweep("reschedule request "+row.Token, err)
		}
	}
}

// expireThreadRequests is the retention pass: answers nobody collected are
// dropped a day after they were written, rows are deleted at the floor, and
// answer files nobody read go with them.
func (a *App) expireThreadRequests(now time.Time) {
	if dropped, err := a.store.ExpireThreadRequestAnswers(now.UnixMilli()); err != nil {
		logThreadRequestSweep("expire answers", err)
	} else if dropped > 0 {
		log.Printf("thread tools: dropped %d uncollected answer(s) past their hold", dropped)
	}
	floor := now.AddDate(0, 0, -store.ThreadRequestRetentionDays).UnixMilli()
	if deleted, err := a.store.DeleteThreadRequestsBefore(floor); err != nil {
		logThreadRequestSweep("delete requests past retention", err)
	} else if deleted > 0 {
		log.Printf("thread tools: deleted %d request row(s) past the %d-day floor", deleted, store.ThreadRequestRetentionDays)
	}
	a.sweepThreadAnswerExports(now)
}

// sweepThreadAnswerExports deletes answer files a day after they were
// written. The tool tells the model to read the file in that same reply, so
// one still there a day later was never read; transcript exports are a
// person's own artefact and are left alone.
func (a *App) sweepThreadAnswerExports(now time.Time) {
	if a.configDir == "" {
		return
	}
	dir := filepath.Join(a.configDir, threadExportDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			logThreadRequestSweep("read export directory", err)
		}
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), threadAnswerExportPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < threadRequestExportAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			logThreadRequestSweep("remove stale answer file", err)
		}
	}
}

// threadAnswerExportPrefix distinguishes one request's answer from a whole
// transcript in the same directory.
const threadAnswerExportPrefix = "answer-"

// sweepThreadRequestsAtBoot settles what the last run left open.
//
// A receipt in `accepted` or `running` describes work that was either queued
// and never sent, or running in a process that is gone. Neither can settle
// itself any more, so both settle here as `interrupted` and their callers are
// told. The scratch threads go too: an ask's fork exists only to answer one
// request, and every request it could answer has just been settled.
func (a *App) sweepThreadRequestsAtBoot() {
	receipts, err := a.store.ListOpenThreadRequestReceipts()
	if err != nil {
		logThreadRequestSweep("list open receipts at boot", err)
	}
	for _, receipt := range receipts {
		func() {
			unlock := a.threadRequestSettleLock(receipt.Token)
			defer unlock()
			if err := a.settleThreadReceipt(receipt.Token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
				State:      store.ThreadReceiptInterrupted,
				Answer:     []byte("This computer restarted before the request finished."),
				AnswerKind: store.ThreadAnswerError,
			}); err != nil {
				logThreadRequestSweep("settle interrupted receipt "+receipt.Token, err)
			}
		}()
	}
	rows, err := a.store.ListScratchThreads()
	if err != nil {
		logThreadRequestSweep("list scratch threads at boot", err)
		return
	}
	for _, row := range rows {
		if err := a.DeleteThread(row.ThreadID); err != nil {
			logThreadRequestSweep("delete scratch thread "+row.ThreadID, err)
			continue
		}
		if _, err := a.store.DeleteScratchThread(row.ThreadID); err != nil {
			logThreadRequestSweep("delete scratch row "+row.ThreadID, err)
		}
	}
}

// logThreadRequestSweep reports what an unattended pass could not do. Nothing
// here has a caller to return an error to, and a silent sweep failure is a
// request that never settles.
func logThreadRequestSweep(what string, err error) {
	if err == nil {
		return
	}
	log.Printf("thread tools sweep: %s: %v", what, err)
}

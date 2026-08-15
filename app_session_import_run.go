package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/project"
	"agent-overflow/internal/sessionimport"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"
)

// sessionImportProgressChannel carries one frame per session an import run
// finishes, plus exactly one terminal frame. It is loopback-only
// (internal/transport/event_visibility.go): the frames name provider-home
// file paths.
const sessionImportProgressChannel = "session-import:progress"

// Per-session outcomes on the progress channel.
const (
	sessionImportStatusImported = "imported"
	sessionImportStatusFailed   = "failed"
	sessionImportStatusSkipped  = "skipped"
)

// ImportSessionsRequest names the rows to import by their opaque scan ids.
type ImportSessionsRequest struct {
	IDs []string `json:"ids"`
}

// ImportRunHandle is what ImportSessions returns immediately; everything else
// arrives on sessionImportProgressChannel.
type ImportRunHandle struct {
	ImportID string `json:"importId"`
	Total    int    `json:"total"`
}

// SessionImportProgressEvent is one frame of an import run.
//
// One channel carries both the per-session frames and the terminal one, so a
// listener that sees Done has seen everything: a second "finished" channel
// could arrive before the last per-session frame and leave the UI stuck.
type SessionImportProgressEvent struct {
	ImportID string `json:"importId"`
	// Completed counts frames reported so far, 0..Total, monotonic. On a
	// cancelled run the terminal frame stops short of Total.
	Completed int `json:"completed"`
	Total     int `json:"total"`
	// ID is the row this frame reports on; absent on the terminal frame.
	ID string `json:"id,omitempty"`
	// Status is imported | failed | skipped; absent on the terminal frame.
	Status string `json:"status,omitempty"`
	// ThreadIDs names the thread the session created. It is a slice because
	// the UI applies row additions in one shape; the importer enforces at most
	// one id per provider session.
	ThreadIDs []string `json:"threadIds,omitempty"`
	// Error is user-facing prose.
	Error string `json:"error,omitempty"`
	// Done is true exactly once, on the final frame.
	Done bool `json:"done,omitempty"`
}

// sessionImportRun is one in-flight import. One per app: importing writes
// threads and projects, and two concurrent runs over overlapping ids would
// race the dedup set that makes "Import All" idempotent.
type sessionImportRun struct {
	id     string
	total  int
	cancel context.CancelFunc
}

const (
	// Eight workers overlap small provider-file decodes while a weighted
	// semaphore caps aggregate source bytes admitted at about 128 MiB. Decode
	// amplification is substantial, so a 128 MiB+ transcript takes every slot
	// and runs alone; medium inputs consume proportionally more capacity.
	// Unknown projects are resolved once before dispatch.
	sessionImportWorkers   = 8
	sessionImportSlotBytes = 16 << 20
)

type sessionImportJob struct {
	id         string
	row        sessionimport.Row
	found      bool
	prepareErr error
}

type sessionImportJobResult struct {
	job     sessionImportJob
	outcome sessionimport.ImportOutcome
	err     error
}

// ImportSessions starts an import run and returns immediately. Progress
// arrives on sessionImportProgressChannel, one frame per session plus a
// terminal frame.
//
// Duplicate ids in one request collapse: the scan's dedup happens BEFORE a
// run, so importing one row twice in the same run would create the threads
// twice.
func (a *App) ImportSessions(req ImportSessionsRequest) (ImportRunHandle, error) {
	ids := dedupeImportIDs(req.IDs)
	if len(ids) == 0 {
		return ImportRunHandle{}, fmt.Errorf("import sessions: no sessions were selected")
	}
	if a.store == nil {
		return ImportRunHandle{}, fmt.Errorf("import sessions: no store")
	}

	ctx, cancel := context.WithCancel(a.lifeCtx())
	run := &sessionImportRun{id: uuid.NewString(), total: len(ids), cancel: cancel}

	a.sessionImportMu.Lock()
	// The stopped flag and the WaitGroup Add sit in ONE critical section, and
	// stopSessionImports sets the flag in that same section before it waits.
	// That is what makes "no goroutine joins the WaitGroup after Wait began"
	// structural rather than a matter of call ordering.
	if a.sessionImportStopped {
		a.sessionImportMu.Unlock()
		cancel()
		return ImportRunHandle{}, ErrShuttingDown
	}
	if a.sessionImportActive != nil {
		a.sessionImportMu.Unlock()
		cancel()
		return ImportRunHandle{}, fmt.Errorf(
			"import sessions: an import is already running; wait for it to finish or cancel it first")
	}
	a.sessionImportActive = run
	a.sessionImportWG.Add(1)
	a.sessionImportMu.Unlock()

	go func() {
		defer a.sessionImportWG.Done()
		defer cancel()
		defer a.finishSessionImportRun(run)
		a.runSessionImport(ctx, run, ids)
	}()

	return ImportRunHandle{ImportID: run.id, Total: run.total}, nil
}

// CancelSessionImport stops the named run. The run still emits its terminal
// frame, with Completed short of Total — a listener never has to infer the
// end from silence.
func (a *App) CancelSessionImport(importID string) error {
	importID = strings.TrimSpace(importID)
	a.sessionImportMu.Lock()
	run := a.sessionImportActive
	a.sessionImportMu.Unlock()
	if run == nil || (importID != "" && run.id != importID) {
		return fmt.Errorf("cancel session import: no import run %q is in progress", importID)
	}
	run.cancel()
	return nil
}

// runSessionImport prepares selected sessions in request order and reports
// each import as it completes; independent small sessions may finish out of
// order because their provider reads and conversion overlap.
//
// Every session is isolated: one that fails is reported and the run carries
// on, because ImportOne already rolls its own session back whole and an
// "Import All" that aborted on the first unreadable transcript would be
// unusable on a real provider home.
func (a *App) runSessionImport(ctx context.Context, run *sessionImportRun, ids []string) {
	completed := 0
	report := func(frame SessionImportProgressEvent) {
		completed++
		frame.ImportID = run.id
		frame.Completed = completed
		frame.Total = run.total
		a.emit(sessionImportProgressChannel, frame)
	}

	deps, err := a.sessionImportDeps()
	if err != nil {
		// Nothing can be imported without the provider homes. Every id fails
		// with the same cause rather than the run ending silently.
		for _, id := range ids {
			report(SessionImportProgressEvent{ID: id, Status: sessionImportStatusFailed, Error: err.Error()})
		}
		a.emitSessionImportDone(run, completed)
		return
	}

	rows := a.resolveImportRows(ctx, ids)
	jobs := make([]sessionImportJob, 0, len(ids))
	type projectResolution struct {
		id  string
		err error
	}
	projectsByPath := make(map[string]projectResolution)
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		row, found := rows[id]
		job := sessionImportJob{id: id, row: row, found: found}
		// Resolve unknown projects before workers start. Besides avoiding a
		// lookup/create race between two sessions from the same repository,
		// this means project creation is not treated as a reason to serialize
		// every import into a fresh store. ImportOne sees the stamped id and
		// retains its normal fallback if a project is deleted meanwhile.
		if found && strings.TrimSpace(row.ProjectID) == "" {
			path := strings.TrimSpace(row.ProjectPath)
			resolved, ok := projectsByPath[path]
			if !ok {
				proj, err := project.EnsureForWorkspace(deps.Store, row.ProjectPath)
				resolved = projectResolution{err: err}
				if err == nil {
					resolved.id = proj.ID
				}
				projectsByPath[path] = resolved
			}
			if resolved.err != nil {
				job.prepareErr = fmt.Errorf("sessionimport: resolve project for %s: %w", row.ID, resolved.err)
			} else {
				job.row.ProjectID = resolved.id
			}
		}
		jobs = append(jobs, job)
	}
	imported := 0
	for result := range runBoundedSessionImports(ctx, deps, jobs, sessionimport.ImportOne) {
		id := result.job.id
		if !result.job.found {
			report(SessionImportProgressEvent{
				ID:     id,
				Status: sessionImportStatusSkipped,
				Error: "This session is no longer available to import — it has either been imported already " +
					"or its session file is gone.",
			})
			continue
		}
		switch {
		case result.err != nil && ctx.Err() != nil:
			// Cancelled mid-session. ImportOne rolled the session back, so
			// there is nothing to report about it; the terminal frame below
			// tells the caller the run stopped early.
		case result.err != nil:
			report(SessionImportProgressEvent{
				ID: id, Status: sessionImportStatusFailed, Error: result.err.Error(),
			})
		case len(result.outcome.Threads) == 0:
			logImportWarnings(id, result.outcome.Warnings)
			report(SessionImportProgressEvent{
				ID: id, Status: sessionImportStatusSkipped,
				Error: "This session contains no importable conversation history.",
			})
		default:
			imported++
			logImportWarnings(id, result.outcome.Warnings)
			report(SessionImportProgressEvent{
				ID: id, Status: sessionImportStatusImported, ThreadIDs: result.outcome.ThreadIDs(),
			})
		}
	}
	if imported > 0 {
		// The imported sessions are no longer importable. Dropping the cached
		// scans keeps the next listing from offering them again for up to a
		// TTL — the scan would subtract them, but only once it re-runs.
		a.sessionImportScanCache().Reset()
	}
	a.emitSessionImportDone(run, completed)
}

// runBoundedSessionImports overlaps the read/build half of independent
// sessions while the Store's single writer connection serializes commits.
// Results arrive in completion order so a small session can advance progress
// while an earlier large transcript is still converting.
//
// The executor argument is the test seam for the scheduler itself; production
// always passes sessionimport.ImportOne.
func runBoundedSessionImports(
	ctx context.Context,
	deps sessionimport.Deps,
	jobs []sessionImportJob,
	execute func(context.Context, sessionimport.Deps, sessionimport.Row) (sessionimport.ImportOutcome, error),
) <-chan sessionImportJobResult {
	results := make(chan sessionImportJobResult, sessionImportWorkers)
	work := make(chan sessionImportJob)
	gate := semaphore.NewWeighted(sessionImportWorkers)

	var workers sync.WaitGroup
	workers.Add(sessionImportWorkers)
	for range sessionImportWorkers {
		go func() {
			defer workers.Done()
			for job := range work {
				if !job.found {
					results <- sessionImportJobResult{job: job}
					continue
				}
				if job.prepareErr != nil {
					results <- sessionImportJobResult{job: job, err: job.prepareErr}
					continue
				}
				weight := sessionImportWeight(job.row.SizeBytes)
				if err := gate.Acquire(ctx, weight); err != nil {
					results <- sessionImportJobResult{job: job, err: err}
					continue
				}
				outcome, err := execute(ctx, deps, job.row)
				gate.Release(weight)
				results <- sessionImportJobResult{job: job, outcome: outcome, err: err}
			}
		}()
	}

	go func() {
		defer close(work)
		for _, job := range jobs {
			select {
			case work <- job:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()
	return results
}

func sessionImportWeight(sizeBytes int64) int64 {
	weight := (sizeBytes + sessionImportSlotBytes - 1) / sessionImportSlotBytes
	if weight < 1 {
		return 1
	}
	if weight > sessionImportWorkers {
		return sessionImportWorkers
	}
	return weight
}

// resolveImportRows maps the requested ids back to scanned rows.
//
// Ids come from a listing the frontend already has, so the cache normally
// answers all of them. A miss means the cached scan expired, and it must NOT
// mean "gone": one rescan re-mints the same ids, because a row id is
// (provider, session id) and nothing about it depends on when the scan ran.
func (a *App) resolveImportRows(ctx context.Context, ids []string) map[string]sessionimport.Row {
	cache := a.sessionImportScanCache()
	rows := make(map[string]sessionimport.Row, len(ids))
	missing := false
	for _, id := range ids {
		if row, ok := cache.Lookup(id); ok {
			rows[id] = row
			continue
		}
		missing = true
	}
	if !missing || ctx.Err() != nil {
		return rows
	}
	if _, err := cache.Get(ctx, true); err != nil {
		// The rescan failed, so the ids it would have resolved stay missing
		// and their sessions are reported as skipped with the cause the
		// caller can act on. Nothing else in the run depends on it.
		return rows
	}
	for _, id := range ids {
		if _, have := rows[id]; have {
			continue
		}
		if row, ok := cache.Lookup(id); ok {
			rows[id] = row
		}
	}
	return rows
}

func (a *App) emitSessionImportDone(run *sessionImportRun, completed int) {
	a.emit(sessionImportProgressChannel, SessionImportProgressEvent{
		ImportID:  run.id,
		Completed: completed,
		Total:     run.total,
		Done:      true,
	})
}

// finishSessionImportRun clears the registry slot, but only when it still
// holds THIS run — a shutdown that cleared it first must not be undone.
func (a *App) finishSessionImportRun(run *sessionImportRun) {
	a.sessionImportMu.Lock()
	if a.sessionImportActive == run {
		a.sessionImportActive = nil
	}
	a.sessionImportMu.Unlock()
}

// stopSessionImports cancels the in-flight run and joins its goroutine.
// Called from Shutdown before the store closes, because an import writes to
// it. Idempotent.
func (a *App) stopSessionImports() {
	a.sessionImportMu.Lock()
	a.sessionImportStopped = true
	run := a.sessionImportActive
	a.sessionImportActive = nil
	a.sessionImportMu.Unlock()
	if run != nil {
		run.cancel()
	}
	a.sessionImportWG.Wait()
}

// importWarningLogLimit bounds one session's logged warnings. A corrupt
// transcript can warn per row, and the point of the log line is that the
// import degraded, not a transcript-length dump.
const importWarningLogLimit = 5

// logImportWarnings records what a reader had to skip or repair.
//
// These never reach the progress frame — the contract has no field for them
// and a per-row list is not something the modal can act on — but they are the
// only trace that an imported thread is missing content, so they must not be
// dropped on the floor.
func logImportWarnings(id string, warnings []importir.Warning) {
	if len(warnings) == 0 {
		return
	}
	shown := warnings
	if len(shown) > importWarningLogLimit {
		shown = shown[:importWarningLogLimit]
	}
	for _, warning := range shown {
		log.Printf("session import %s: %s: %s", id, warning.Code, warning.Message)
	}
	if len(warnings) > len(shown) {
		log.Printf("session import %s: %d further warning(s) not shown", id, len(warnings)-len(shown))
	}
}

// dedupeImportIDs trims, drops blanks, and keeps the first occurrence of each
// id in request order.
func dedupeImportIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

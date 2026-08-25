//go:build !providersmoke

package main

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/sessionimport"
	"agent-overflow/internal/store"
)

// progressRecorder captures the session-import progress channel through the
// same testEmitHook seam every other App event test uses, and lets a test
// block until the run's terminal frame lands.
type progressRecorder struct {
	mu     sync.Mutex
	frames []SessionImportProgressEvent
	done   chan struct{}
	closed bool
}

func recordSessionImportProgress(app *App) *progressRecorder {
	rec := &progressRecorder{done: make(chan struct{})}
	app.testEmitHook = func(name string, data any) {
		if name != sessionImportProgressChannel {
			return
		}
		frame, ok := data.(SessionImportProgressEvent)
		if !ok {
			return
		}
		rec.mu.Lock()
		rec.frames = append(rec.frames, frame)
		if frame.Done && !rec.closed {
			rec.closed = true
			close(rec.done)
		}
		rec.mu.Unlock()
	}
	return rec
}

func (r *progressRecorder) wait(t *testing.T) []SessionImportProgressEvent {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("import run never emitted its terminal frame")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SessionImportProgressEvent(nil), r.frames...)
}

// runImport starts a run and blocks until it finishes, returning its frames.
func runImport(t *testing.T, app *App, ids ...string) []SessionImportProgressEvent {
	t.Helper()
	rec := recordSessionImportProgress(app)
	handle, err := app.ImportSessions(ImportSessionsRequest{IDs: ids})
	if err != nil {
		t.Fatalf("ImportSessions: %v", err)
	}
	if handle.ImportID == "" || handle.Total != len(ids) {
		t.Fatalf("handle = %+v, want an id and total %d", handle, len(ids))
	}
	frames := rec.wait(t)
	last := frames[len(frames)-1]
	if !last.Done || last.ImportID != handle.ImportID {
		t.Fatalf("terminal frame = %+v, want done for run %s", last, handle.ImportID)
	}
	return frames
}

func TestListImportableSessionsReportsBothProviders(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeLinearSession(t, importFixtureClaudeSession)
	home.writeCodexIndex(t, importFixtureCodexThread)
	home.codexLinearSession(t, importFixtureCodexThread)

	result, err := app.ListImportableSessions(ImportScanRequest{})
	if err != nil {
		t.Fatalf("ListImportableSessions: %v", err)
	}
	if len(result.Providers) != 2 {
		t.Fatalf("providers = %+v, want one entry per provider", result.Providers)
	}
	for _, status := range result.Providers {
		if !status.Available || status.Error != "" {
			t.Fatalf("provider %s unavailable: %+v", status.Provider, status)
		}
	}
	if result.ScannedAt <= 0 {
		t.Fatalf("scannedAt = %d, want the ms the disk was read", result.ScannedAt)
	}

	byID := map[string]ImportableSession{}
	for _, row := range result.Rows {
		byID[row.ID] = row
	}
	claudeRow, ok := byID["claude:"+importFixtureClaudeSession]
	if !ok {
		t.Fatalf("rows = %+v, want the claude row keyed provider:sessionID", result.Rows)
	}
	codexRow, ok := byID["codex:"+importFixtureCodexThread]
	if !ok {
		t.Fatalf("rows = %+v, want the codex row keyed provider:sessionID", result.Rows)
	}
	for _, row := range []ImportableSession{claudeRow, codexRow} {
		if row.ProjectPath != home.workspace {
			t.Errorf("%s projectPath = %q, want the session cwd %q", row.ID, row.ProjectPath, home.workspace)
		}
		if row.ProjectLabel == "" {
			t.Errorf("%s has no project label; the frontend must not derive its own", row.ID)
		}
		if row.SourcePath == "" || row.LastActivityAt <= 0 {
			t.Errorf("%s = %+v, want a source path and an activity time", row.ID, row)
		}
	}
}

// A broken home for one provider must not take the other's sessions away —
// that is why the scan reports availability per provider instead of failing.
func TestListImportableSessionsKeepsOneProviderWhenTheOtherIsUnreadable(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeLinearSession(t, importFixtureClaudeSession)
	// A Codex home that exists but holds no thread index: the schema moved or
	// the install is broken, which is NOT the same as "no Codex sessions".

	result, err := app.ListImportableSessions(ImportScanRequest{})
	if err != nil {
		t.Fatalf("ListImportableSessions: %v", err)
	}
	statuses := map[string]ImportProviderStatus{}
	for _, status := range result.Providers {
		statuses[status.Provider] = status
	}
	if !statuses[sessionimport.ProviderClaude].Available {
		t.Fatalf("claude unavailable: %+v", statuses[sessionimport.ProviderClaude])
	}
	codex := statuses[sessionimport.ProviderCodex]
	if codex.Available || codex.Error == "" {
		t.Fatalf("codex status = %+v, want unavailable with user-facing prose", codex)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("rows = %d, want the claude row to survive a broken codex home", len(result.Rows))
	}
}

// wireImportScanClock installs a scan cache driven by a clock the test moves,
// and returns the clock plus a counter of how many disk walks happened.
func wireImportScanClock(app *App, ttl time.Duration) (*time.Time, *int) {
	now := time.Unix(1000, 0)
	scans := 0
	app.sessionImport.scans = sessionimport.NewScanCache(ttl,
		func() time.Time { return now },
		func(ctx context.Context) (sessionimport.ScanResult, error) {
			scans++
			return app.scanImportableSessions(ctx)
		})
	return &now, &scans
}

func TestListImportableSessionsCachesUntilForceRefreshOrTTL(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeLinearSession(t, importFixtureClaudeSession)
	now, scans := wireImportScanClock(app, time.Minute)

	for i := 0; i < 3; i++ {
		if _, err := app.ListImportableSessions(ImportScanRequest{}); err != nil {
			t.Fatalf("list %d: %v", i, err)
		}
	}
	if *scans != 1 {
		t.Fatalf("scans = %d, want 1 (the TTL should cover all three lists)", *scans)
	}
	if _, err := app.ListImportableSessions(ImportScanRequest{ForceRefresh: true}); err != nil {
		t.Fatalf("force refresh: %v", err)
	}
	if *scans != 2 {
		t.Fatalf("scans = %d, want 2 after ForceRefresh", *scans)
	}
	// Past the TTL the entry is stale and the next list walks the disk again.
	// The clock is injected precisely so this is a real expiry rather than a
	// Reset() standing in for one.
	*now = now.Add(2 * time.Minute)
	if _, err := app.ListImportableSessions(ImportScanRequest{}); err != nil {
		t.Fatalf("post-expiry list: %v", err)
	}
	if *scans != 3 {
		t.Fatalf("scans = %d, want 3 after the TTL expired", *scans)
	}
}

// Lookup honors the TTL, and a miss must not read as "this session is gone":
// the import run rescans and re-mints the same ids.
func TestImportScanCacheLookupExpiresWithTheEntry(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeLinearSession(t, importFixtureClaudeSession)
	now, scans := wireImportScanClock(app, time.Minute)

	if _, err := app.ListImportableSessions(ImportScanRequest{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	cache := app.sessionImportScanCache()
	id := "claude:" + importFixtureClaudeSession
	row, ok := cache.Lookup(id)
	if !ok || row.SourcePath == "" {
		t.Fatalf("Lookup(%q) = %+v, %v; want the freshly scanned row", id, row, ok)
	}

	*now = now.Add(2 * time.Minute)
	if _, ok := cache.Lookup(id); ok {
		t.Fatal("Lookup answered from an expired entry; the row's path and size may have changed on disk")
	}
	if *scans != 1 {
		t.Fatalf("scans = %d, want 1 — Lookup must never walk the disk itself", *scans)
	}

	// A miss is the run's cue to rescan, and the rescan re-mints the same id.
	rows := app.resolveImportRows(context.Background(), []string{id})
	if _, found := rows[id]; !found {
		t.Fatalf("resolveImportRows = %+v, want the id back after the forced rescan", rows)
	}
	if *scans != 2 {
		t.Fatalf("scans = %d, want 2 (one rescan for the expired lookup)", *scans)
	}
}

func TestImportSessionsCreatesThreadsAndReportsProgress(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeLinearSession(t, importFixtureClaudeSession)
	home.writeCodexIndex(t, importFixtureCodexThread)
	home.codexLinearSession(t, importFixtureCodexThread)

	if _, err := app.ListImportableSessions(ImportScanRequest{}); err != nil {
		t.Fatalf("list before import: %v", err)
	}
	claudeID := "claude:" + importFixtureClaudeSession
	codexID := "codex:" + importFixtureCodexThread
	frames := runImport(t, app, claudeID, codexID)

	if len(frames) != 3 {
		t.Fatalf("frames = %+v, want one per session plus the terminal frame", frames)
	}
	for i, frame := range frames {
		if frame.Total != 2 {
			t.Errorf("frame %d total = %d, want 2", i, frame.Total)
		}
	}
	// Completed counts sessions settled, so the two per-session frames run
	// 1..2 and the terminal frame repeats the final count.
	for i, want := range []int{1, 2, 2} {
		if frames[i].Completed != want {
			t.Errorf("frame %d completed = %d, want %d (monotonic, terminal repeats)",
				i, frames[i].Completed, want)
		}
	}
	for _, frame := range frames[:2] {
		if frame.Status != sessionImportStatusImported {
			t.Fatalf("frame %+v: want imported", frame)
		}
		if len(frame.ThreadIDs) != 1 {
			t.Fatalf("frame %+v: want exactly one created thread id", frame)
		}
	}
	if frames[2].ID != "" || frames[2].Status != "" {
		t.Errorf("terminal frame = %+v, want no per-row fields", frames[2])
	}

	threads, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	sources := map[string]string{}
	for _, thread := range threads {
		sources[thread.ImportSource] = thread.ID
	}
	if sources[sessionimport.ProviderClaude] == "" || sources[sessionimport.ProviderCodex] == "" {
		t.Fatalf("imported threads = %+v, want one per provider stamped with importSource", threads)
	}
	// Imported history keeps the provider's own clock; nothing restamps now().
	for _, thread := range threads {
		if thread.CreatedAt > importFixtureMillis+60_000 {
			t.Errorf("thread %s createdAt = %d, want the session's own time near %d",
				thread.ID, thread.CreatedAt, importFixtureMillis)
		}
	}
}

func TestRunBoundedSessionImportsOverlapsSmallSessionsAndRunsLargeOnesAlone(t *testing.T) {
	initialStarted := make(chan string, sessionImportWorkers)
	releaseInitial := make(chan struct{})
	largeStarted := make(chan struct{}, 1)
	releaseLarge := make(chan struct{})

	jobs := make([]sessionImportJob, 0, sessionImportWorkers+3)
	for i := range sessionImportWorkers {
		id := "initial-" + strconv.Itoa(i)
		jobs = append(jobs, sessionImportJob{
			id: id, found: true,
			row: sessionimport.Row{ID: id, ProjectID: "project", SizeBytes: 1},
		})
	}
	jobs = append(jobs,
		sessionImportJob{id: "large", found: true, row: sessionimport.Row{
			ID: "large", ProjectID: "project", SizeBytes: sessionImportSlotBytes * sessionImportWorkers,
		}},
		sessionImportJob{id: "later-a", found: true, row: sessionimport.Row{ID: "later-a", ProjectID: "project", SizeBytes: 1}},
		sessionImportJob{id: "later-b", found: true, row: sessionimport.Row{ID: "later-b", ProjectID: "project", SizeBytes: 1}},
	)

	// Exclusivity is measured as an OVERLAP, not as a moment. The three
	// trailing jobs reach the semaphore in whatever order the workers pick
	// them up: "later-a" can legitimately acquire its one slot, run and
	// finish before "large" ever asks for its full-width weight. Sampling a
	// started-channel after the large import begins cannot tell that apart
	// from a real overlap, which is what made the older form of this test
	// fail under load. What the weights actually promise is that nothing
	// else is IN execute while the large import is, so that is what this
	// records.
	var mu sync.Mutex
	running := 0
	largeRunning := false
	overlapped := ""
	enter := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		running++
		isLarge := id == "large"
		if overlapped == "" && (largeRunning || (isLarge && running > 1)) {
			overlapped = id
		}
		if isLarge {
			largeRunning = true
		}
	}
	leave := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		running--
		if id == "large" {
			largeRunning = false
		}
	}

	execute := func(
		_ context.Context, _ sessionimport.Deps, row sessionimport.Row,
	) (sessionimport.ImportOutcome, error) {
		enter(row.ID)
		defer leave(row.ID)
		switch {
		case strings.HasPrefix(row.ID, "initial-"):
			initialStarted <- row.ID
			<-releaseInitial
		case row.ID == "large":
			largeStarted <- struct{}{}
			<-releaseLarge
		}
		return sessionimport.ImportOutcome{}, nil
	}

	results := runBoundedSessionImports(context.Background(), sessionimport.Deps{}, jobs, execute)
	for range sessionImportWorkers {
		select {
		case <-initialStarted:
		case <-time.After(time.Second):
			t.Fatalf("%d small imports did not overlap", sessionImportWorkers)
		}
	}
	// Safe as an instant check: every slot is held by a small import that is
	// parked on releaseInitial, so a large import that had started would mean
	// the semaphore handed out more weight than it has.
	select {
	case <-largeStarted:
		t.Fatal("large import started while the small imports still held every slot")
	default:
	}
	close(releaseInitial)
	select {
	case <-largeStarted:
	case <-time.After(time.Second):
		t.Fatal("large import did not start after the small imports released their slots")
	}
	close(releaseLarge)

	var completed int
	for result := range results {
		if result.err != nil {
			t.Fatalf("job %s: %v", result.job.id, result.err)
		}
		completed++
	}
	if completed != len(jobs) {
		t.Fatalf("completed jobs = %d, want %d", completed, len(jobs))
	}
	mu.Lock()
	defer mu.Unlock()
	if overlapped != "" {
		t.Fatalf("import %s ran while the exclusive large import held every slot", overlapped)
	}
}

func TestSessionImportWeightCapsAggregateSourceBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		want int64
	}{
		{name: "unknown", size: 0, want: 1},
		{name: "one byte", size: 1, want: 1},
		{name: "one slot", size: sessionImportSlotBytes, want: 1},
		{name: "over one slot", size: sessionImportSlotBytes + 1, want: 2},
		{name: "three slots", size: 3 * sessionImportSlotBytes, want: 3},
		{name: "full budget", size: sessionImportWorkers * sessionImportSlotBytes, want: sessionImportWorkers},
		{name: "oversize", size: 10 * sessionImportWorkers * sessionImportSlotBytes, want: sessionImportWorkers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionImportWeight(tc.size); got != tc.want {
				t.Fatalf("sessionImportWeight(%d) = %d, want %d", tc.size, got, tc.want)
			}
		})
	}
}

// Pressing Import twice must be a no-op, not a duplicate: the scan subtracts
// sessions AO already has, so the second run finds no row for the id.
func TestImportSessionsSkipsAlreadyImportedSessions(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeLinearSession(t, importFixtureClaudeSession)

	if _, err := app.ListImportableSessions(ImportScanRequest{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	id := "claude:" + importFixtureClaudeSession
	runImport(t, app, id)

	frames := runImport(t, app, id)
	if frames[0].Status != sessionImportStatusSkipped {
		t.Fatalf("second run frame = %+v, want skipped", frames[0])
	}
	if frames[0].Error == "" {
		t.Error("a skipped session must say why; a silent skip reads as a successful import")
	}
	threads, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 1 {
		t.Fatalf("threads = %d, want the second run to create nothing", len(threads))
	}
}

func TestImportSessionsReportsMetadataOnlyActiveHistoryAsSkipped(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	const sessionID = "dddddddd-4444-4444-8444-dddddddddddd"
	home.writeClaudeSession(t, sessionID,
		home.claudeUserRow("u1", "", "inactive conversation", 0),
		home.claudeAssistantRow("a1", "u1", "msg-1", "Inactive answer.", 1_000),
		// The last leaf is the session's active chain and contains no event the
		// importer can render. The inactive conversation must not substitute.
		map[string]any{
			"type": "attachment", "uuid": "att1", "parentUuid": nil,
			"isSidechain": false, "timestamp": importFixtureISO(2_000),
			"cwd":        home.workspace,
			"attachment": map[string]any{"type": "file_history", "content": "…"},
		},
	)

	if _, err := app.ListImportableSessions(ImportScanRequest{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	frames := runImport(t, app, "claude:"+sessionID)
	if frames[0].Status != sessionImportStatusSkipped || frames[0].Error == "" {
		t.Fatalf("frame = %+v, want a user-visible skipped outcome", frames[0])
	}
	if len(frames[0].ThreadIDs) != 0 {
		t.Fatalf("thread ids = %v, want none", frames[0].ThreadIDs)
	}
	threads, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 0 {
		t.Fatalf("threads = %d, want none", len(threads))
	}
}

// One session named three ways in ONE request is one import. The dedup
// happens before the run because the scan's own dedup only subtracts sessions
// AO already has — within a single run nothing would stop the second mention
// from importing the same transcript a second time, as a second thread.
func TestImportSessionsCollapsesDuplicateIDsInOneRequest(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeLinearSession(t, importFixtureClaudeSession)

	if _, err := app.ListImportableSessions(ImportScanRequest{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	id := "claude:" + importFixtureClaudeSession

	rec := recordSessionImportProgress(app)
	handle, err := app.ImportSessions(ImportSessionsRequest{IDs: []string{id, "  " + id + "  ", id}})
	if err != nil {
		t.Fatalf("ImportSessions: %v", err)
	}
	if handle.Total != 1 {
		t.Fatalf("handle total = %d, want 1 — the three ids name one session", handle.Total)
	}
	frames := rec.wait(t)
	if len(frames) != 2 {
		t.Fatalf("frames = %+v, want one per-session frame plus the terminal one", frames)
	}
	if frames[0].Status != sessionImportStatusImported {
		t.Fatalf("frame = %+v, want the one session imported", frames[0])
	}
	threads, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 1 {
		t.Fatalf("threads = %d, want one — a repeated id must not import the transcript twice", len(threads))
	}
}

func TestImportSessionsRejectsBlankSelection(t *testing.T) {
	app := newTestAppWithStore(t)
	if _, err := app.ImportSessions(ImportSessionsRequest{IDs: []string{"  ", ""}}); err == nil {
		t.Fatal("ImportSessions with no usable ids = nil error, want a refusal")
	}
}

// blockingScanCache installs a scan the test controls, so a run can be held
// in flight while the concurrency and cancellation contracts are asserted.
//
// The empty fixture home is what makes the run REACH that scan: a run resolves
// the provider homes before it resolves any id, and sessionImportDeps refuses
// to fall back to the real home inside a test binary — without it every id
// would fail up front and the run would never park.
func blockingScanCache(t *testing.T, app *App) (release func()) {
	t.Helper()
	newImportHome(t).attach(app)
	gate := make(chan struct{})
	app.sessionImport.scans = sessionimport.NewScanCache(time.Minute, time.Now,
		func(ctx context.Context) (sessionimport.ScanResult, error) {
			select {
			case <-gate:
				return sessionimport.ScanResult{}, nil
			case <-ctx.Done():
				return sessionimport.ScanResult{}, ctx.Err()
			}
		})
	return sync.OnceFunc(func() { close(gate) })
}

func TestImportSessionsRefusesASecondRunWhileOneIsActive(t *testing.T) {
	app := newTestAppWithStore(t)
	release := blockingScanCache(t, app)
	defer release()
	rec := recordSessionImportProgress(app)

	first, err := app.ImportSessions(ImportSessionsRequest{IDs: []string{"claude:unknown"}})
	if err != nil {
		t.Fatalf("first ImportSessions: %v", err)
	}
	// The run is parked in the id-resolution rescan until release().
	if _, err := app.ImportSessions(ImportSessionsRequest{IDs: []string{"claude:other"}}); err == nil {
		t.Fatal("second ImportSessions = nil error, want a refusal while a run is active")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second ImportSessions error = %v, want it to say a run is in progress", err)
	}

	release()
	frames := rec.wait(t)
	if frames[len(frames)-1].ImportID != first.ImportID {
		t.Fatalf("terminal frame = %+v, want the first run's id", frames[len(frames)-1])
	}

	// Once the first run has settled the registry slot is free again.
	if _, err := app.ImportSessions(ImportSessionsRequest{IDs: []string{"claude:other"}}); err != nil {
		t.Fatalf("third ImportSessions after the run settled: %v", err)
	}
}

func TestCancelSessionImportStopsTheRunAndStillReportsDone(t *testing.T) {
	app := newTestAppWithStore(t)
	release := blockingScanCache(t, app)
	defer release()
	rec := recordSessionImportProgress(app)

	handle, err := app.ImportSessions(ImportSessionsRequest{IDs: []string{"claude:a", "claude:b"}})
	if err != nil {
		t.Fatalf("ImportSessions: %v", err)
	}
	if err := app.CancelSessionImport("some-other-run"); err == nil {
		t.Fatal("CancelSessionImport with a foreign id = nil error, want a refusal")
	}
	if err := app.CancelSessionImport(handle.ImportID); err != nil {
		t.Fatalf("CancelSessionImport: %v", err)
	}

	frames := rec.wait(t)
	last := frames[len(frames)-1]
	if !last.Done || last.ImportID != handle.ImportID {
		t.Fatalf("terminal frame = %+v, want done for the cancelled run", last)
	}
	if last.Completed >= last.Total {
		t.Fatalf("cancelled run reported %d/%d; a cancel must stop short", last.Completed, last.Total)
	}
	threads, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 0 {
		t.Fatalf("threads = %d, want none from a cancelled run", len(threads))
	}
}

// The row ids the frontend holds must outlive the scan cache: an id is
// (provider, session id) and nothing about it depends on when the scan ran.
func TestImportSessionsResolvesIDsAfterTheScanCacheExpires(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeLinearSession(t, importFixtureClaudeSession)

	if _, err := app.ListImportableSessions(ImportScanRequest{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	app.sessionImportScanCache().Reset()

	frames := runImport(t, app, "claude:"+importFixtureClaudeSession)
	if frames[0].Status != sessionImportStatusImported {
		t.Fatalf("frame = %+v, want the id to survive cache expiry via a rescan", frames[0])
	}
}

// importOneClaudeSession is the shared arrangement for the refresh and
// branch-resume tests: scan, import, and hand back the created threads.
func importOneClaudeSession(t *testing.T, app *App, home importHome, sessionID string) []store.Thread {
	t.Helper()
	if _, err := app.ListImportableSessions(ImportScanRequest{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	frames := runImport(t, app, "claude:"+sessionID)
	if frames[0].Status != sessionImportStatusImported {
		t.Fatalf("import frame = %+v, want imported", frames[0])
	}
	threads := make([]store.Thread, 0, len(frames[0].ThreadIDs))
	for _, id := range frames[0].ThreadIDs {
		thread, err := app.store.GetThread(id)
		if err != nil {
			t.Fatalf("GetThread(%s): %v", id, err)
		}
		threads = append(threads, thread)
	}
	return threads
}

package sessionimport

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/project"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"
)

const (
	ImportStatusImported = "imported"
	ImportStatusFailed   = "failed"
	ImportStatusSkipped  = "skipped"

	managerWorkers   = 8
	managerSlotBytes = 16 << 20
)

// ManagerConfig supplies the app-owned boundaries around session import.
// Provider homes remain caller-resolved: this package never falls through to
// the process user's real home.
type ManagerConfig struct {
	Context       func() context.Context
	ResolveDeps   func() (Deps, error)
	ValidateStart func() error
	LockThread    func(string) func()
	EmitProgress  func(ProgressEvent)
	ShutdownError error
	ScanTTL       time.Duration
	Now           func() time.Time
	// Scan is an optional deterministic loader seam. Production leaves it nil.
	Scan func(context.Context, Deps, Filter) (ScanResult, error)
}

// Manager owns the cached provider-home scan and the one active import run.
// Its lifecycle boundary guarantees that Stop joins every store-writing
// goroutine before the caller closes the store.
type Manager struct {
	config ManagerConfig

	cacheOnce sync.Once
	cache     *ScanCache

	mu      sync.Mutex
	active  *managerRun
	stopped bool
	wg      sync.WaitGroup

	scan          func(context.Context, Deps, Filter) (ScanResult, error)
	importOne     func(context.Context, Deps, Row) (ImportOutcome, error)
	planUpdate    func(context.Context, Deps, string) (Update, error)
	applyUpdate   func(Deps, Update) (ApplyResult, error)
	ensureProject func(*store.Store, string) (store.Project, error)
}

// RunHandle identifies an asynchronous import run.
type RunHandle struct {
	ImportID string
	Total    int
}

// ProgressEvent is one per-session result or the terminal run frame.
type ProgressEvent struct {
	ImportID  string
	Completed int
	Total     int
	ID        string
	Status    string
	ThreadIDs []string
	Error     string
	Done      bool
}

// UpdateStatus is the stable, wire-neutral projection of an update plan.
type UpdateStatus struct {
	ThreadID             string
	Status               string
	NewItems             int
	NewTurns             int
	RestoresModelProfile bool
	Detail               string
}

// AppliedUpdate reports what ApplyThreadUpdates committed.
type AppliedUpdate struct {
	Items                int
	Turns                int
	RestoredModelProfile bool
}

type managerRun struct {
	id     string
	total  int
	cancel context.CancelFunc
}

type managerJob struct {
	id         string
	row        Row
	found      bool
	prepareErr error
}

type managerJobResult struct {
	job     managerJob
	outcome ImportOutcome
	err     error
}

// NewManager constructs a session-import coordinator with no process-global
// state. Config callbacks may be called from background import goroutines.
func NewManager(config ManagerConfig) *Manager {
	if config.Context == nil {
		config.Context = context.Background
	}
	if config.LockThread == nil {
		config.LockThread = func(string) func() { return func() {} }
	}
	if config.EmitProgress == nil {
		config.EmitProgress = func(ProgressEvent) {}
	}
	if config.ShutdownError == nil {
		config.ShutdownError = errors.New("session import is shutting down")
	}
	if config.ScanTTL <= 0 {
		config.ScanTTL = ScanTTL
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	manager := &Manager{
		config:        config,
		importOne:     ImportOne,
		planUpdate:    PlanUpdate,
		applyUpdate:   ApplyUpdate,
		ensureProject: project.EnsureForWorkspace,
	}
	manager.scan = config.Scan
	if manager.scan == nil {
		manager.scan = Scan
	}
	return manager
}

func (m *Manager) deps() (Deps, error) {
	if m == nil || m.config.ResolveDeps == nil {
		return Deps{}, fmt.Errorf("session import: dependencies are unavailable")
	}
	return m.config.ResolveDeps()
}

func (m *Manager) scanCache() *ScanCache {
	m.cacheOnce.Do(func() {
		if m.cache == nil {
			m.cache = NewScanCache(m.config.ScanTTL, m.config.Now, func(ctx context.Context) (ScanResult, error) {
				deps, err := m.deps()
				if err != nil {
					return ScanResult{}, err
				}
				return m.scan(ctx, deps, Filter{})
			})
		}
	})
	return m.cache
}

// List returns the cached or freshly scanned import catalogue.
func (m *Manager) List(force bool) (CachedScan, error) {
	return m.scanCache().Get(m.config.Context(), force)
}

// CheckThreadUpdates plans an exact, read-only refresh under the same thread
// lock used by ApplyThreadUpdates.
func (m *Manager) CheckThreadUpdates(threadID string) (UpdateStatus, error) {
	threadID = strings.TrimSpace(threadID)
	deps, err := m.deps()
	if err != nil {
		return UpdateStatus{}, err
	}
	unlock := m.config.LockThread(threadID)
	defer unlock()
	update, err := m.planUpdate(m.config.Context(), deps, threadID)
	if err != nil {
		return UpdateStatus{}, err
	}
	return UpdateStatus{
		ThreadID:             update.ThreadID,
		Status:               update.Status,
		NewItems:             update.NewItems,
		NewTurns:             update.NewTurns,
		RestoresModelProfile: update.RestoresModelProfile(),
		Detail:               update.Detail,
	}, nil
}

// ApplyThreadUpdates re-plans and commits a refresh while holding the thread
// lock; a status returned earlier is never trusted as a write plan.
func (m *Manager) ApplyThreadUpdates(threadID string) (AppliedUpdate, error) {
	threadID = strings.TrimSpace(threadID)
	deps, err := m.deps()
	if err != nil {
		return AppliedUpdate{}, err
	}
	unlock := m.config.LockThread(threadID)
	defer unlock()
	update, err := m.planUpdate(m.config.Context(), deps, threadID)
	if err != nil {
		return AppliedUpdate{}, err
	}
	result, err := m.applyUpdate(deps, update)
	if err != nil {
		return AppliedUpdate{}, err
	}
	logImportWarnings(threadID, update.Warnings)
	return AppliedUpdate{Items: result.Items, Turns: result.Turns, RestoredModelProfile: result.RestoredModelProfile}, nil
}

// Start begins one import run. Duplicate and blank ids are removed while
// retaining first-seen request order.
func (m *Manager) Start(ids []string) (RunHandle, error) {
	ids = dedupeImportIDs(ids)
	if len(ids) == 0 {
		return RunHandle{}, fmt.Errorf("import sessions: no sessions were selected")
	}
	if m.config.ValidateStart != nil {
		if err := m.config.ValidateStart(); err != nil {
			return RunHandle{}, err
		}
	}
	ctx, cancel := context.WithCancel(m.config.Context())
	run := &managerRun{id: uuid.NewString(), total: len(ids), cancel: cancel}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		cancel()
		return RunHandle{}, m.config.ShutdownError
	}
	if m.active != nil {
		m.mu.Unlock()
		cancel()
		return RunHandle{}, fmt.Errorf("import sessions: an import is already running; wait for it to finish or cancel it first")
	}
	m.active = run
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer cancel()
		defer m.finish(run)
		m.run(ctx, run, ids)
	}()
	return RunHandle{ImportID: run.id, Total: run.total}, nil
}

// Cancel stops the named run; the run still emits one terminal frame.
func (m *Manager) Cancel(importID string) error {
	importID = strings.TrimSpace(importID)
	m.mu.Lock()
	run := m.active
	m.mu.Unlock()
	if run == nil || (importID != "" && run.id != importID) {
		return fmt.Errorf("cancel session import: no import run %q is in progress", importID)
	}
	run.cancel()
	return nil
}

// Reset invalidates the provider-home listing cache.
func (m *Manager) Reset() { m.scanCache().Reset() }

// Stop prevents new runs, cancels the active run, and joins it. It is
// idempotent and must complete before the backing store is closed.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.stopped = true
	run := m.active
	m.active = nil
	m.mu.Unlock()
	if run != nil {
		run.cancel()
	}
	m.wg.Wait()
}

func (m *Manager) run(ctx context.Context, run *managerRun, ids []string) {
	completed := 0
	report := func(frame ProgressEvent) {
		completed++
		frame.ImportID, frame.Completed, frame.Total = run.id, completed, run.total
		m.config.EmitProgress(frame)
	}
	deps, err := m.deps()
	if err != nil {
		for _, id := range ids {
			report(ProgressEvent{ID: id, Status: ImportStatusFailed, Error: err.Error()})
		}
		m.emitDone(run, completed)
		return
	}
	rows := m.resolveRows(ctx, ids)
	jobs := make([]managerJob, 0, len(ids))
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
		job := managerJob{id: id, row: row, found: found}
		if found && strings.TrimSpace(row.ProjectID) == "" {
			path := strings.TrimSpace(row.ProjectPath)
			resolved, ok := projectsByPath[path]
			if !ok {
				proj, resolveErr := m.ensureProject(deps.Store, row.ProjectPath)
				resolved = projectResolution{err: resolveErr}
				if resolveErr == nil {
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
	for result := range m.runBounded(ctx, deps, jobs) {
		id := result.job.id
		if !result.job.found {
			report(ProgressEvent{ID: id, Status: ImportStatusSkipped, Error: "This session is no longer available to import — it has either been imported already or its session file is gone."})
			continue
		}
		switch {
		case result.err != nil && ctx.Err() != nil:
		case result.err != nil:
			report(ProgressEvent{ID: id, Status: ImportStatusFailed, Error: result.err.Error()})
		case len(result.outcome.Threads) == 0:
			logImportWarnings(id, result.outcome.Warnings)
			report(ProgressEvent{ID: id, Status: ImportStatusSkipped, Error: "This session contains no importable conversation history."})
		default:
			imported++
			logImportWarnings(id, result.outcome.Warnings)
			report(ProgressEvent{ID: id, Status: ImportStatusImported, ThreadIDs: result.outcome.ThreadIDs()})
		}
	}
	if imported > 0 {
		m.Reset()
	}
	m.emitDone(run, completed)
}

func (m *Manager) runBounded(ctx context.Context, deps Deps, jobs []managerJob) <-chan managerJobResult {
	results := make(chan managerJobResult, managerWorkers)
	work := make(chan managerJob)
	gate := semaphore.NewWeighted(managerWorkers)
	var workers sync.WaitGroup
	workers.Add(managerWorkers)
	for range managerWorkers {
		go func() {
			defer workers.Done()
			for job := range work {
				if !job.found {
					results <- managerJobResult{job: job}
					continue
				}
				if job.prepareErr != nil {
					results <- managerJobResult{job: job, err: job.prepareErr}
					continue
				}
				weight := managerWeight(job.row.SizeBytes)
				if err := gate.Acquire(ctx, weight); err != nil {
					results <- managerJobResult{job: job, err: err}
					continue
				}
				outcome, err := m.importOne(ctx, deps, job.row)
				gate.Release(weight)
				results <- managerJobResult{job: job, outcome: outcome, err: err}
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
	go func() { workers.Wait(); close(results) }()
	return results
}

func managerWeight(sizeBytes int64) int64 {
	weight := (sizeBytes + managerSlotBytes - 1) / managerSlotBytes
	if weight < 1 {
		return 1
	}
	if weight > managerWorkers {
		return managerWorkers
	}
	return weight
}

func (m *Manager) resolveRows(ctx context.Context, ids []string) map[string]Row {
	cache := m.scanCache()
	rows := make(map[string]Row, len(ids))
	missing := false
	for _, id := range ids {
		if row, ok := cache.Lookup(id); ok {
			rows[id] = row
		} else {
			missing = true
		}
	}
	if !missing || ctx.Err() != nil {
		return rows
	}
	if _, err := cache.Get(ctx, true); err != nil {
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

func (m *Manager) emitDone(run *managerRun, completed int) {
	m.config.EmitProgress(ProgressEvent{ImportID: run.id, Completed: completed, Total: run.total, Done: true})
}

func (m *Manager) finish(run *managerRun) {
	m.mu.Lock()
	if m.active == run {
		m.active = nil
	}
	m.mu.Unlock()
}

const importWarningLogLimit = 5

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

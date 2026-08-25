package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/memory"
	"agent-overflow/internal/workflow/profile"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

// fakeHost is the runner's host seam with every capability a settable func and
// a harmless default. It exists because the seams are what the runner's
// contract IS: a test states the two or three capabilities its subject actually
// reaches and leaves the rest inert, which is the whole reason the runner stopped
// holding `*App`.
//
// The git half is deliberately a REAL `gitops.Core`, because the provisioning
// rules under test are decided by what git answers.
// The App's own implementations of those four seams are thin wrappers over the
// same Core (`app_worktree.go`), so a test that drives them here drives the same
// git.
type fakeHost struct {
	startSession           func(context.Context, string) error
	startSessionTakingLock func(context.Context, string) error
	stopSession            func(string) error
	sessionActive          func(string) bool
	subscribe              func(string, func(string, provider.ProviderEvent)) func()
	send                   func(context.Context, string, string, json.RawMessage, func(DispatchIdentity)) error
	createThread           func(ThreadSpec) (store.Thread, error)
	assistantTexts         func(string) ([]string, error)
	// dataStore lets the default CreateWorkflowThread persist a real row, so a
	// start that runs past thread creation has a thread id its own store can
	// resolve. newTestRunner wires it to the runner's store.
	dataStore      *store.Store
	gitCore        *gitops.Core
	branchPrefix   string
	configDir      string
	promptAncestry func(string, def.Workflow) workflowrunner.PromptContext
	recordMemory   func(engine.RunKey, []memory.Draft)
	emit           func(eventchan.Channel, any)
	requireEngine  func() (*engine.Engine, error)
	lifeCtx        context.Context

	observerMu     sync.Mutex
	observers      map[string]map[int]func(string, provider.ProviderEvent)
	nextObserverID int
}

func (h *fakeHost) StartSession(ctx context.Context, threadID string) error {
	if h.startSession == nil {
		return nil
	}
	return h.startSession(ctx, threadID)
}

func (h *fakeHost) StartSessionTakingLock(ctx context.Context, threadID string) error {
	if h.startSessionTakingLock == nil {
		return nil
	}
	return h.startSessionTakingLock(ctx, threadID)
}

func (h *fakeHost) StopSession(threadID string) error {
	if h.stopSession == nil {
		return nil
	}
	return h.stopSession(threadID)
}

func (h *fakeHost) SessionActive(threadID string) bool {
	return h.sessionActive != nil && h.sessionActive(threadID)
}

// SubscribeThreadTurnObserver defaults to a real per-thread observer bus, the
// shape `App.subscribeThreadTurnObserver` / `dispatchTurnObservers` are: a test
// that resubscribes (the refused takeover does) can only tell a fresh
// subscription from a dangling reference by dispatching through it.
func (h *fakeHost) SubscribeThreadTurnObserver(
	threadID string, observer func(string, provider.ProviderEvent),
) func() {
	if h.subscribe != nil {
		return h.subscribe(threadID, observer)
	}
	h.observerMu.Lock()
	defer h.observerMu.Unlock()
	if h.observers == nil {
		h.observers = make(map[string]map[int]func(string, provider.ProviderEvent))
	}
	if h.observers[threadID] == nil {
		h.observers[threadID] = make(map[int]func(string, provider.ProviderEvent))
	}
	h.nextObserverID++
	id := h.nextObserverID
	h.observers[threadID][id] = observer
	return func() {
		h.observerMu.Lock()
		defer h.observerMu.Unlock()
		delete(h.observers[threadID], id)
	}
}

// dispatchTurnObservers delivers one provider event to every live subscription
// on a thread, the way the App's own dispatcher does.
func (h *fakeHost) dispatchTurnObservers(threadID string, event provider.ProviderEvent) {
	h.observerMu.Lock()
	observers := make([]func(string, provider.ProviderEvent), 0, len(h.observers[threadID]))
	for _, observer := range h.observers[threadID] {
		observers = append(observers, observer)
	}
	h.observerMu.Unlock()
	for _, observer := range observers {
		observer(threadID, event)
	}
}

func (h *fakeHost) SendWorkflowMessage(
	ctx context.Context, threadID, content string,
	outputSchema json.RawMessage, onDispatch func(DispatchIdentity),
) error {
	if h.send == nil {
		return nil
	}
	return h.send(ctx, threadID, content, outputSchema, onDispatch)
}

// CreateWorkflowThread persists the minimum of what the App's own version does:
// the identity, workspace, and provider fields the runner (and the store rows it
// writes next) read back. The model-profile seeding and access→runtime-mode
// mapping the App applies are App policy, tested in `main`.
func (h *fakeHost) CreateWorkflowThread(spec ThreadSpec) (store.Thread, error) {
	if h.createThread != nil {
		return h.createThread(spec)
	}
	if h.dataStore == nil {
		return store.Thread{}, nil
	}
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID: uuid.NewString(), ProjectID: spec.Workspace.Project.ID,
		ProjectPath: spec.Workspace.Project.Path, Title: spec.Title,
		Provider: spec.ProviderName, Model: provider.NormalizeModelSlug(spec.ProviderName, spec.Model),
		WorkspacePath: spec.Workspace.Path, Mode: "workflow",
		CreatedAt: now, UpdatedAt: now,
	}
	if !gitops.SameFilesystemPath(spec.Workspace.Path, spec.Workspace.Project.Path) {
		thread.WorktreePath = spec.Workspace.Path
		thread.Branch = spec.Workspace.Branch
	}
	if err := h.dataStore.CreateThread(thread); err != nil {
		return store.Thread{}, err
	}
	return h.dataStore.GetThread(thread.ID)
}

func (h *fakeHost) ThreadAssistantTexts(threadID string) ([]string, error) {
	if h.assistantTexts == nil {
		return nil, nil
	}
	return h.assistantTexts(threadID)
}

// GitCore hands out a real Core, built on first use. A test that reaches git at
// all wants the real answers; the field exists so a test that needs a
// pre-configured Core can supply one.
func (h *fakeHost) GitCore() *gitops.Core {
	if h.gitCore == nil {
		h.gitCore = gitops.NewCore()
	}
	return h.gitCore
}

// FindWorktree, CutWorktreeFromFreshBase and DefaultWorktreePath mirror
// `app_worktree.go`'s bodies against the same Core, so what the runner is held
// to here is what production does.
func (h *fakeHost) FindWorktree(project, path string) (gitops.Worktree, bool, error) {
	worktrees, err := h.GitCore().ListWorktrees(project)
	if err != nil {
		return gitops.Worktree{}, false, err
	}
	for _, worktree := range worktrees {
		if gitops.SameFilesystemPath(worktree.Path, path) {
			return worktree, true, nil
		}
	}
	return gitops.Worktree{}, false, nil
}

func (h *fakeHost) CutWorktreeFromFreshBase(
	ctx context.Context, projectPath, worktreePath, baseBranch, newBranch string,
) error {
	_, err := h.GitCore().CreateWorktreeFromFreshBase(ctx, projectPath, worktreePath, baseBranch, newBranch)
	return err
}

func (h *fakeHost) DefaultWorktreePath(projectPath, branch string) (string, error) {
	base := gitops.DefaultWorktreesBaseDir(projectPath)
	if h.configDir != "" {
		base = filepath.Join(h.configDir, "worktrees", filepath.Base(projectPath))
	}
	return gitops.UniqueWorktreePath(filepath.Join(base, gitops.SanitizeWorktreePathSegment(branch)))
}

func (h *fakeHost) WorktreeBranchPrefix() string {
	if h.branchPrefix == "" {
		return gitops.AutoWorktreeBranchPrefix
	}
	return h.branchPrefix
}

func (h *fakeHost) WorkflowPromptAncestry(
	itemID string, workflow def.Workflow,
) workflowrunner.PromptContext {
	if h.promptAncestry == nil {
		return workflowrunner.PromptContext{}
	}
	return h.promptAncestry(itemID, workflow)
}

func (h *fakeHost) RecordEnvelopeMemory(key engine.RunKey, drafts []memory.Draft) {
	if h.recordMemory != nil {
		h.recordMemory(key, drafts)
	}
}

func (h *fakeHost) Emit(name eventchan.Channel, data any) {
	if h.emit != nil {
		h.emit(name, data)
	}
}

func (h *fakeHost) RequireWorkflowEngine() (*engine.Engine, error) {
	if h.requireEngine == nil {
		return nil, errNoEngineInTest
	}
	return h.requireEngine()
}

func (h *fakeHost) LifeCtx() context.Context {
	if h.lifeCtx == nil {
		return context.Background()
	}
	return h.lifeCtx
}

var errNoEngineInTest = errors.New("workflowhost test: no engine wired")

// newTestRunner builds a runner over a fake host and, unless the caller passes
// nil, a migrated store cloned from the package template. The interrupt seam
// defaults to a successful no-op — the tests that care what an interrupt does
// (or that it failed) assign `runner.interrupt` themselves.
func newTestRunner(t *testing.T, host *fakeHost, dataStore *store.Store, profiles engine.ProfileSource) *Runner {
	t.Helper()
	// The runner never spawns — every process it would start is behind a host
	// seam — but the continuation preflight reads a provider home directly
	// (`claude.ScanSessionLeaf`). Detaching HOME here is what keeps that read off
	// the developer's real `~/.claude`, and the poisoned binary is the tripwire
	// if a path ever does reach for a CLI.
	kerneltest.IsolateSpawns(t)
	if host == nil {
		host = &fakeHost{}
	}
	if host.dataStore == nil {
		host.dataStore = dataStore
	}
	return New(host, dataStore, t.TempDir(), profiles, func(context.Context, string) error { return nil })
}

// newTestStore clones the package's migrated template. Several runner paths
// (the typed usage-limit park, unit and phase attachment) write rows, and a nil
// store turns an assertion into a nil-pointer panic that reports as a crashed
// binary rather than as the regression it caught.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.Clone(t)
}

type staticWorkflowProfileSource struct{ value *profile.Profile }

func (s staticWorkflowProfileSource) Profile(context.Context, string) (*profile.Profile, error) {
	return s.value, nil
}

type fakeWorkflowTimer struct {
	callback func()
	delay    time.Duration
	active   bool
	resets   []time.Duration
}

func (t *fakeWorkflowTimer) Stop() bool {
	wasActive := t.active
	t.active = false
	return wasActive
}

func (t *fakeWorkflowTimer) Reset(delay time.Duration) bool {
	wasActive := t.active
	t.delay = delay
	t.active = true
	t.resets = append(t.resets, delay)
	return wasActive
}

func (t *fakeWorkflowTimer) fire() { t.callback() }

// schemaForThread reads the phase schema registered for a thread WITHOUT the
// takeover fallthrough `SessionSchemaForThread` performs — that one clears
// `schemaAttached` as a side effect, which makes it useless as an assertion
// probe.
func (r *Runner) schemaForThread(threadID string) json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append(json.RawMessage(nil), r.schemas[threadID]...)
}

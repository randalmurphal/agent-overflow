package workflowapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/serialqueue"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/memory"
	"agent-overflow/internal/workflow/scheduler"
	"agent-overflow/internal/workflowdefs"
	"agent-overflow/internal/workflowhost"
	"agent-overflow/internal/workflowwatch"
)

const workflowSourceAgent = "agent"

// Deps names the narrow runtime/application seams the persisted read surface
// still needs. Store-backed projection behavior belongs to Service; callbacks
// remain only where the owning runtime concern has not moved yet.
type Deps struct {
	Store               *store.Store
	DataRoot            func() string
	RunBudget           func(context.Context, store.WorkItem) (*Budget, error)
	MemoryProvenance    func(threadID, phaseID string) memory.Provenance
	RecordMemory        func(store.WorkItem, memory.Provenance, []memory.Draft) (int, error)
	MemoryTree          func(store.WorkItem) (MemoryTree, error)
	Git                 func() Git
	Context             func() context.Context
	ListBranchCommits   ListBranchCommitsFunc
	ProjectProfile      func(projectID string) (DispositionProfile, error)
	ParkDisposition     func(itemID string) error
	ResolveDisposition  func(itemID string) error
	CancelRun           func(itemID string) error
	RemoveOtherWorktree func(projectID, path string) error
	InvalidateWorkspace func(path string)
	EmitState           func(engine.StateEvent)
	EmitError           func(engine.ErrorEvent)
	EnsureWorkflowReady func() error
	LockTriage          func(itemID string) func()
	NewTriageThread     func(TriageThreadInput) store.Thread
	SendThreadMessage   func(threadID, message string) error
	DeleteThread        func(threadID string) error
	Digest              func(store.WorkItem, string, json.RawMessage, string) Digest
	GenerateDigest      func(context.Context, store.WorkItem, Digest) (Digest, error)
	Lifecycle           LifecyclePorts
	WakeDelivery        WakeDeliveryPorts
	Attention           AttentionPorts
	ResumeRun           func(context.Context, string) error
	Logf                func(format string, args ...any)
	Now                 func() time.Time
}

// Service owns the store-backed workflow agent read surface. It has no
// process-global state and registers nothing on the wire.
type Service struct {
	deps               Deps
	dispositionMu      sync.Mutex
	runtimeMu          sync.RWMutex
	engine             *engine.Engine
	runner             *workflowhost.Runner
	scheduler          *scheduler.Scheduler
	autoDisposition    serialqueue.Queue
	schedulerQueue     serialqueue.Queue
	wake               serialqueue.Queue
	autoResume         autoResumeState
	digestMu           sync.Mutex
	digestSlots        chan struct{}
	watch              workflowwatch.Hub
	definitionsWatcher *workflowdefs.Watcher
}

// HandleEngineEvent preserves the engine-event ordering contract: prepare
// durable projection state, emit to the transport/replay bus, then schedule
// application reactions.
func (s *Service) HandleEngineEvent(name eventchan.Channel, payload any, emit func(eventchan.Channel, any)) {
	s.prepareEngineEvent(name, payload)
	emit(name, payload)
	s.afterEngineEvent(name, payload)
}

func (s *Service) WaitWake() { s.wake.Wait() }

func New(deps Deps) *Service {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Context == nil {
		deps.Context = context.Background
	}
	if deps.Logf == nil {
		deps.Logf = log.Printf
	}
	return &Service{deps: deps}
}

func (s *Service) store() (*store.Store, error) {
	if s == nil || s.deps.Store == nil {
		return nil, errors.New("workflow application: store unavailable")
	}
	return s.deps.Store, nil
}

func (s *Service) dataRoot() string {
	if s == nil || s.deps.DataRoot == nil {
		return ""
	}
	return s.deps.DataRoot()
}

func requireCallerScope(ctx context.Context) (transport.CallerScope, error) {
	scope, ok := transport.CallerScopeFrom(ctx)
	if !ok {
		return transport.CallerScope{}, fmt.Errorf(
			"this method is part of the ao CLI surface and requires a session-scoped token")
	}
	if strings.TrimSpace(scope.ProjectID) == "" {
		return transport.CallerScope{}, fmt.Errorf("this session is not attached to a project")
	}
	return scope, nil
}

func phaseSourceRefPrefix(scope transport.CallerScope) string {
	return scope.ItemID + "/" + scope.PhaseID + "/"
}

func (s *Service) scopedRun(scope transport.CallerScope, itemID, action string, readOnly bool) (store.WorkItem, error) {
	database, err := s.store()
	if err != nil {
		return store.WorkItem{}, err
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return store.WorkItem{}, fmt.Errorf("%s: run id is required", action)
	}
	item, err := database.GetWorkItemSummary(itemID)
	if err != nil {
		return store.WorkItem{}, err
	}
	if item.ProjectID != scope.ProjectID {
		return store.WorkItem{}, fmt.Errorf("%s: run %s belongs to another project", action, itemID)
	}
	if !scope.IsPhase() || readOnly && scope.HasGrant(string(def.GrantIntrospect)) {
		return item, nil
	}
	if item.Source == workflowSourceAgent && strings.HasPrefix(item.SourceRef, phaseSourceRefPrefix(scope)) {
		return item, nil
	}
	return store.WorkItem{}, fmt.Errorf(
		"%s: run %s was not started by phase %q of run %s; this phase may only act on the runs it started",
		action, itemID, scope.PhaseID, scope.ItemID)
}

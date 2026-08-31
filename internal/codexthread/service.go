package codexthread

import (
	"context"
	"sync"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
)

// LiveSession is the Codex portion of one root-owned provider session.
type LiveSession struct {
	Token   string
	Session *codex.Session
}

// Deps supplies root-owned lifecycle, session, store, and event seams.
type Deps struct {
	Context        func() context.Context
	IsShuttingDown func() bool
	ShutdownError  error
	Session        func(threadID string) (LiveSession, bool)
	Store          *store.Store
	Emit           func(eventchan.Channel, any)
}

// Service owns Codex provider-thread reconciliation and cumulative-cost reads.
type Service struct {
	deps  Deps
	store *store.Store

	costMu       sync.Mutex
	costInflight map[string]*threadCostRead
}

// threadCostRead is one thread's in-flight cumulative-cost read slot.
type threadCostRead struct {
	dirty bool
	token string
	epoch uint64
}

// New constructs a Codex provider-thread service.
func New(deps Deps) *Service {
	return &Service{deps: deps, store: deps.Store}
}

func (a *Service) lifeCtx() context.Context {
	if a != nil && a.deps.Context != nil {
		return a.deps.Context()
	}
	return context.Background()
}

func (a *Service) isShuttingDown() bool {
	return a != nil && a.deps.IsShuttingDown != nil && a.deps.IsShuttingDown()
}

func (a *Service) shutdownError() error {
	if a != nil && a.deps.ShutdownError != nil {
		return a.deps.ShutdownError
	}
	return context.Canceled
}

func (a *Service) session(threadID string) (LiveSession, bool) {
	if a == nil || a.deps.Session == nil {
		return LiveSession{}, false
	}
	return a.deps.Session(threadID)
}

func (a *Service) emit(channel eventchan.Channel, payload any) {
	if a != nil && a.deps.Emit != nil {
		a.deps.Emit(channel, payload)
	}
}

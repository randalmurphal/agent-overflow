// Package worktreesetupapp owns the asynchronous chat-worktree setup runtime.
package worktreesetupapp

import (
	"context"
	"errors"
	"sync"

	"agent-overflow/internal/store"
	"agent-overflow/internal/worktreesetup"
)

// Store is the durable slice required by setup coordination.
type Store interface {
	GetThread(string) (store.Thread, error)
	ProjectWorktreeSetup(string) (worktreesetup.Config, bool, error)
	SetThreadWorktreeSetupState(string, string) error
	SweepRunningThreadWorktreeSetups() (int64, error)
}

// Events receives committed setup and thread-row projections.
type Events interface {
	Setup(Event)
	ThreadUpdated(store.Thread)
}

type Config struct {
	Store         Store
	Events        Events
	Context       func() context.Context
	ShutdownError error
}

// Service owns every setup run and every ward protecting run lifecycle.
type Service struct {
	store         Store
	events        Events
	context       func() context.Context
	shutdownError error

	mu      sync.Mutex
	runs    map[string]*worktreeSetupRun
	stopped bool
	wg      sync.WaitGroup
}

func New(config Config) *Service {
	contextSource := config.Context
	if contextSource == nil {
		contextSource = context.Background
	}
	shutdownError := config.ShutdownError
	if shutdownError == nil {
		shutdownError = errors.New("worktree setup service stopped")
	}
	return &Service{
		store:         config.Store,
		events:        config.Events,
		context:       contextSource,
		shutdownError: shutdownError,
		runs:          make(map[string]*worktreeSetupRun),
	}
}

func (s *Service) emitSetup(event Event) {
	if s.events != nil {
		s.events.Setup(event)
	}
}

func (s *Service) emitThreadUpdated(thread store.Thread) {
	if s.events != nil {
		s.events.ThreadUpdated(thread)
	}
}

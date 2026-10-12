package workflowapp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowhost"
)

// EngineRuntimeConfig is the composition boundary for the persisted workflow
// engine. workflowapp owns the live engine and runner after StartEngine
// succeeds; root still constructs the provider-aware Runner and the definition,
// profile, spend, logging, and transport adapters they require.
type EngineRuntimeConfig struct {
	BeginWork   func(context.Context) (func(), error)
	Runner      *workflowhost.Runner
	Definitions engine.DefinitionSource
	Profiles    engine.ProfileSource
	Spend       engine.SpendSource
	Paused      bool
	Log         engine.LogSink
	Emit        func(eventchan.Channel, any)
}

type runtimeEmitter struct {
	service *Service
	emit    func(eventchan.Channel, any)
}

func (e runtimeEmitter) Emit(name eventchan.Channel, payload any) {
	e.service.HandleEngineEvent(name, payload, e.emit)
}

// StartEngine creates, publishes, and starts the one workflow engine. Publishing
// before Start matches the engine's recovery contract: callbacks emitted during
// the synchronous rebuild must see the same runtime that owns them. A failed
// start clears both pointers before returning.
func (s *Service) StartEngine(ctx context.Context, config EngineRuntimeConfig) error {
	if config.Runner == nil || config.Emit == nil {
		return fmt.Errorf("workflow runtime: runner and emitter are required")
	}
	workflowEngine, err := engine.New(
		s.deps.Store, config.Runner, runtimeEmitter{service: s, emit: config.Emit},
		config.Definitions, config.Profiles, config.Spend,
		engine.Config{Paused: config.Paused, Log: config.Log, BeginWork: config.BeginWork},
	)
	if err != nil {
		return err
	}
	s.runtimeMu.Lock()
	if s.engine != nil || s.runner != nil {
		s.runtimeMu.Unlock()
		return fmt.Errorf("workflow runtime: engine already initialized")
	}
	s.engine = workflowEngine
	s.runner = config.Runner
	s.runtimeMu.Unlock()
	if err := workflowEngine.Start(ctx); err != nil {
		s.runtimeMu.Lock()
		s.engine = nil
		s.runner = nil
		s.runtimeMu.Unlock()
		return err
	}
	return nil
}

func (s *Service) Engine() *engine.Engine {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.engine
}

func (s *Service) Runner() *workflowhost.Runner {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.runner
}

// SetRunnerForTesting installs an isolated runner without starting an engine.
// Production runtime ownership is established only by StartEngine.
func (s *Service) SetRunnerForTesting(runner *workflowhost.Runner) {
	s.runtimeMu.Lock()
	s.runner = runner
	s.runtimeMu.Unlock()
}

func (s *Service) HasEngine() bool { return s.Engine() != nil }
func (s *Service) HasRunner() bool { return s.Runner() != nil }

// CloseEngine stops event production, then drains every application reaction
// queue that can still write SQLite. Root calls this before closing the store.
func (s *Service) CloseEngine() error {
	workflowEngine := s.Engine()
	if workflowEngine == nil {
		return nil
	}
	err := workflowEngine.Close()
	s.autoDisposition.Wait()
	s.WaitWake()
	return err
}

func (s *Service) AbortEngine() error {
	err := s.CloseEngine()
	s.runtimeMu.Lock()
	s.engine = nil
	s.runner = nil
	s.runtimeMu.Unlock()
	return err
}

func (s *Service) queueAutoDisposition(itemID string) {
	if s.deps.Lifecycle.AutoDispose == nil {
		return
	}
	s.autoDisposition.Go(func() { s.deps.Lifecycle.AutoDispose(itemID) })
}

func (s *Service) WaitAutoDisposition() { s.autoDisposition.Wait() }

// Runner-specific integration methods keep the concrete Runner inside the
// application service while allowing the general session/send paths to honor
// workflow-owned provider sessions.
func (s *Service) SessionSchemaForThread(threadID string) (json.RawMessage, bool) {
	runner := s.Runner()
	if runner == nil {
		return nil, false
	}
	return runner.SessionSchemaForThread(threadID)
}

func (s *Service) SessionDisconnected(threadID string) {
	if runner := s.Runner(); runner != nil {
		runner.SessionDisconnected(threadID)
	}
}

func (s *Service) RegisterTakeover(ctx context.Context, itemID, threadID string) error {
	runner := s.Runner()
	if runner == nil {
		return fmt.Errorf("workflow runner unavailable")
	}
	return runner.RegisterTakeover(ctx, itemID, threadID)
}

func (s *Service) BeginTakeoverTransition(ctx context.Context, itemID, threadID string) error {
	runner := s.Runner()
	if runner == nil {
		return fmt.Errorf("workflow runner unavailable")
	}
	return runner.BeginTakeoverTransition(ctx, itemID, threadID)
}

func (s *Service) CancelTakeoverTransition(itemID, threadID string) {
	if runner := s.Runner(); runner != nil {
		runner.CancelTakeoverTransition(itemID, threadID)
	}
}

func (s *Service) ClearTakeover(itemID string) {
	if runner := s.Runner(); runner != nil {
		runner.ClearTakeover(itemID)
	}
}

func (s *Service) ClearTakeoverThread(threadID string) {
	if runner := s.Runner(); runner != nil {
		runner.ClearTakeoverThread(threadID)
	}
}

func (s *Service) DataRoot() string {
	if runner := s.Runner(); runner != nil {
		return runner.DataRoot()
	}
	return ""
}

func (s *Service) WorkspaceLockRefs(itemID string) int {
	if runner := s.Runner(); runner != nil {
		return runner.WorkspaceLockRefs(itemID)
	}
	return 0
}

func (s *Service) SetRunnerStopSendWaitForTesting(wait time.Duration) {
	if runner := s.Runner(); runner != nil {
		runner.StopSendWait = wait
	}
}

func (s *Service) SyncEngine() error {
	if workflowEngine := s.Engine(); workflowEngine != nil {
		return workflowEngine.Sync()
	}
	return nil
}

func (s *Service) PauseActiveForShutdown(timeout time.Duration) error {
	workflowEngine := s.Engine()
	if workflowEngine == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- workflowEngine.PauseAllActive() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("pause active workflow runs: %w", err)
		}
		return nil
	case <-timer.C:
		s.deps.Logf(
			"workflow shutdown: pausing active runs did not finish within %s; "+
				"runs still active will be parked interrupted on the next launch",
			timeout,
		)
		return nil
	}
}

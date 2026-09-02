package gitapp

import (
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"agent-overflow/internal/gitwatch"
)

// MaxStatusHandles bounds app-wide Git status subscription handles. Root
// aliases this value for its stable wire-facing error and contract tests.
const MaxStatusHandles = 256

// ErrTooManyStatusSubscriptions is returned once the app-wide handle cap is
// reached. The cap prevents renderer callers from creating unbounded fs watches.
var ErrTooManyStatusSubscriptions = errors.New("gitwatch: too many active git status subscriptions")

type statusPump struct {
	cwd  string
	sub  *gitwatch.Subscription
	done chan struct{}
	refs int
	dead bool
}

type statusState struct {
	mu      sync.Mutex
	pumps   map[string]*statusPump
	handles map[string]*statusPump
	wg      sync.WaitGroup
}

// Subscribe begins one workspace-keyed status stream and returns its initial
// snapshot. The caller owns connection-lifetime cleanup and must call
// Unsubscribe if it cannot register that cleanup.
func (s *Service) Subscribe(ref WorkspaceRef) (StatusSubscription, error) {
	if s.shuttingDown() {
		if s.shuttingDownError != nil {
			return StatusSubscription{}, s.shuttingDownError
		}
		return StatusSubscription{}, fmt.Errorf("app: shutting down")
	}
	if s.watch == nil {
		return StatusSubscription{}, fmt.Errorf("gitwatch: manager not initialised")
	}
	_, workspace, err := s.ResolveWorkspace(ref)
	if err != nil {
		return StatusSubscription{}, err
	}

	s.status.mu.Lock()
	atCap := len(s.status.handles) >= MaxStatusHandles
	s.status.mu.Unlock()
	if atCap {
		return StatusSubscription{}, ErrTooManyStatusSubscriptions
	}

	s.core.InvalidatePRCache(workspace)
	sub, err := s.watch.Subscribe(workspace)
	if err != nil {
		return StatusSubscription{}, fmt.Errorf("gitwatch subscribe: %w", err)
	}
	cwd, initial := sub.Cwd(), sub.Initial()

	id := uuid.NewString()
	s.status.mu.Lock()
	if len(s.status.handles) >= MaxStatusHandles {
		s.status.mu.Unlock()
		sub.Close()
		return StatusSubscription{}, ErrTooManyStatusSubscriptions
	}
	pump, shared := s.status.pumps[cwd]
	if shared && pump.dead {
		shared = false
	}
	if shared {
		pump.refs++
	} else {
		pump = &statusPump{cwd: cwd, sub: sub, done: make(chan struct{}), refs: 1}
		s.status.pumps[cwd] = pump
	}
	s.status.handles[id] = pump
	s.status.mu.Unlock()

	if shared {
		sub.Close()
	} else {
		s.status.wg.Go(func() { s.pumpStatus(pump) })
	}
	return StatusSubscription{ID: id, Cwd: cwd, Status: initial}, nil
}

// Unsubscribe releases one caller handle. Unknown and repeated ids are no-ops.
func (s *Service) Unsubscribe(id string) {
	s.status.mu.Lock()
	pump, ok := s.status.handles[id]
	if !ok {
		s.status.mu.Unlock()
		return
	}
	delete(s.status.handles, id)
	pump.refs--
	var teardown *statusPump
	if pump.refs <= 0 {
		teardown = pump
		if s.status.pumps[pump.cwd] == pump {
			delete(s.status.pumps, pump.cwd)
		}
	}
	s.status.mu.Unlock()
	if teardown == nil {
		return
	}
	close(teardown.done)
	teardown.sub.Close()
}

func (s *Service) pumpStatus(pump *statusPump) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.log("gitwatch: pump panic for cwd=%s: %v", pump.cwd, recovered)
		}
		s.dropStatusPump(pump)
	}()
	for {
		select {
		case <-pump.done:
			return
		case status, ok := <-pump.sub.Updates():
			if !ok {
				return
			}
			select {
			case <-pump.done:
				return
			default:
			}
			if s.emitStatus != nil {
				s.emitStatus(StatusEvent{Cwd: pump.cwd, Status: status})
			}
		}
	}
}

func (s *Service) dropStatusPump(pump *statusPump) {
	s.status.mu.Lock()
	defer s.status.mu.Unlock()
	pump.dead = true
	if s.status.pumps[pump.cwd] == pump {
		delete(s.status.pumps, pump.cwd)
	}
	for id, held := range s.status.handles {
		if held == pump {
			delete(s.status.handles, id)
		}
	}
}

// CloseStatus stops the underlying watcher manager and joins every wire pump.
// It must run before the store and event transport close.
func (s *Service) CloseStatus() {
	if s.watch == nil {
		return
	}
	s.watch.Close()
	s.status.wg.Wait()
}

// RequestRefresh schedules a status refresh for a subscribed workspace.
func (s *Service) RequestRefresh(workspace string) {
	if s.watch != nil {
		s.watch.RequestRefresh(workspace)
	}
}

// StatusPumpRefsForTesting reports the live shared-pump refcount to legacy root
// integration tests while the race-sensitive unit coverage lives here.
func (s *Service) StatusPumpRefsForTesting(cwd string) (int, bool) {
	s.status.mu.Lock()
	defer s.status.mu.Unlock()
	pump, ok := s.status.pumps[cwd]
	if !ok {
		return 0, false
	}
	return pump.refs, true
}

// StatusPumpCountsForTesting reports current resource counts to root tests.
func (s *Service) StatusPumpCountsForTesting() (pumps, handles int) {
	s.status.mu.Lock()
	defer s.status.mu.Unlock()
	return len(s.status.pumps), len(s.status.handles)
}

package gitapp

import (
	"context"
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
	handles map[string]*statusHandle
	wg      sync.WaitGroup
}

// statusHandle is one caller's reference on a pump.
type statusHandle struct {
	pump *statusPump
	// untie runs, outside the lock, once the handle is gone, however it
	// goes: its Unsubscribe or its pump's end.
	untie func()
}

// StatusTie ties a handle to its owner as Subscribe publishes it. It runs
// in the critical section that publishes the handle, so it must not block
// and must not call back into the Service; an error refuses the handle,
// and nothing is published. Its answer runs once the handle is gone,
// however it goes.
type StatusTie func(id string) (untie func(), err error)

// Subscribe begins one workspace-keyed status stream and returns its initial
// snapshot. tie binds the handle to its owner as it is published, so a
// handle the caller receives is one its owner already holds.
func (s *Service) Subscribe(ctx context.Context, ref WorkspaceRef, tie StatusTie) (StatusSubscription, error) {
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
	sub, err := s.watch.Subscribe(ctx, workspace)
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
	untie, err := tie(id)
	if err != nil {
		s.status.mu.Unlock()
		sub.Close()
		return StatusSubscription{}, err
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
	s.status.handles[id] = &statusHandle{pump: pump, untie: untie}
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
	handle, ok := s.status.handles[id]
	if !ok {
		s.status.mu.Unlock()
		return
	}
	delete(s.status.handles, id)
	pump := handle.pump
	pump.refs--
	var teardown *statusPump
	if pump.refs <= 0 {
		teardown = pump
		if s.status.pumps[pump.cwd] == pump {
			delete(s.status.pumps, pump.cwd)
		}
	}
	s.status.mu.Unlock()
	if handle.untie != nil {
		handle.untie()
	}
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
	pump.dead = true
	if s.status.pumps[pump.cwd] == pump {
		delete(s.status.pumps, pump.cwd)
	}
	var released []func()
	for id, handle := range s.status.handles {
		if handle.pump == pump {
			delete(s.status.handles, id)
			if handle.untie != nil {
				released = append(released, handle.untie)
			}
		}
	}
	s.status.mu.Unlock()
	for _, release := range released {
		release()
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

// RefreshPR asks the forge again for the workspace's open PR and schedules
// a status refresh, which reads that answer from the cache. For a PR just
// created, which nothing on the filesystem announces. The lookup runs
// whether or not a stream watches the workspace, so the merged-PR cleanup
// learns of the PR either way.
func (s *Service) RefreshPR(ctx context.Context, workspace string) {
	s.core.RefreshOpenPR(ctx, workspace)
	s.RequestRefresh(workspace)
}

// RequestRefresh schedules a status refresh for a subscribed workspace.
func (s *Service) RequestRefresh(workspace string) {
	if s.watch != nil {
		s.watch.RequestRefresh(workspace)
	}
}

// refreshRepositoryStatus schedules a status refresh for every subscribed
// workspace of the repository whose common dir is commonDir: its root and
// each linked worktree carry the same GitStatus.RemoteSignInError, and a
// fetch that failed wrote nothing their watchers see.
func (s *Service) refreshRepositoryStatus(commonDir string) {
	if s.watch == nil {
		return
	}
	s.status.mu.Lock()
	cwds := make([]string, 0, len(s.status.pumps))
	for cwd := range s.status.pumps {
		cwds = append(cwds, cwd)
	}
	s.status.mu.Unlock()
	for _, cwd := range cwds {
		// A subscribed workspace git no longer resolves is not this
		// repository's; its own stream reports what became of it.
		if dir, err := s.core.CommonDir(cwd); err == nil && dir == commonDir {
			s.watch.RequestRefresh(cwd)
		}
	}
}

// SuppressStatus holds the status stream of workspace off the wire until
// the returned resume runs or the stream's watcher stops. See
// gitwatch.Manager.Suppress.
func (s *Service) SuppressStatus(workspace string) (resume func()) {
	if s.watch == nil {
		return func() {}
	}
	return s.watch.Suppress(workspace)
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

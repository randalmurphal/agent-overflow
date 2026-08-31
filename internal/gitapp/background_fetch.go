package gitapp

import (
	"context"
	"slices"
	"sync"
	"time"
)

const (
	backgroundFetchInitialDelay = 45 * time.Second
	backgroundFetchTickInterval = time.Minute
	backgroundFetchStoreKey     = "store"
	backgroundFetchPathKey      = "path:"
	backgroundFetchRepoKey      = "repo:"
)

type fetchErrorMemo struct {
	mu   sync.Mutex
	last map[string]string
}

func (m *fetchErrorMemo) shouldLog(key, message string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		m.last = make(map[string]string)
	}
	if previous, ok := m.last[key]; ok && previous == message {
		return false
	}
	m.last[key] = message
	return true
}

func (m *fetchErrorMemo) clear(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.last, key)
}

func (m *fetchErrorMemo) retain(live map[string]struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.last {
		if _, ok := live[key]; !ok {
			delete(m.last, key)
		}
	}
}

type backgroundFetchState struct {
	mu     sync.Mutex
	stop   chan struct{}
	cancel context.CancelFunc
	wg     sync.WaitGroup
	errors fetchErrorMemo
}

// StartBackgroundFetch launches the unattended fetch cadence. The disabled
// switch is root-owned harness policy and is checked before allocating state.
func (s *Service) StartBackgroundFetch(parent context.Context, disabled bool) {
	if disabled {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	s.fetch.mu.Lock()
	if s.fetch.stop != nil {
		s.fetch.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	ctx, cancel := context.WithCancel(parent)
	s.fetch.stop = stop
	s.fetch.cancel = cancel
	s.fetch.wg.Add(1)
	s.fetch.mu.Unlock()

	go func() {
		defer s.fetch.wg.Done()
		defer cancel()
		initial := time.NewTimer(backgroundFetchInitialDelay)
		defer initial.Stop()
		select {
		case <-stop:
			return
		case <-initial.C:
			s.RunBackgroundFetchPass(ctx)
		}
		ticker := time.NewTicker(backgroundFetchTickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.RunBackgroundFetchPass(ctx)
			}
		}
	}()
}

// StopBackgroundFetch cancels the active subprocess before joining the loop.
func (s *Service) StopBackgroundFetch() {
	s.fetch.mu.Lock()
	stop := s.fetch.stop
	cancel := s.fetch.cancel
	if stop != nil {
		close(stop)
	}
	if cancel != nil {
		cancel()
	}
	// Keep the lifecycle lock through the join. Otherwise a concurrent Start
	// could Add to the WaitGroup after this Stop began waiting, or install a
	// successor whose fields this Stop then cleared.
	s.fetch.wg.Wait()
	s.fetch.stop = nil
	s.fetch.cancel = nil
	s.fetch.mu.Unlock()
}

// BackgroundFetchRunningForTesting reports whether the cadence has started.
func (s *Service) BackgroundFetchRunningForTesting() bool {
	s.fetch.mu.Lock()
	defer s.fetch.mu.Unlock()
	return s.fetch.stop != nil
}

// BackgroundFetchErrorKeysForTesting snapshots memoized failure subjects.
func (s *Service) BackgroundFetchErrorKeysForTesting() []string {
	s.fetch.errors.mu.Lock()
	defer s.fetch.errors.mu.Unlock()
	keys := make([]string, 0, len(s.fetch.errors.last))
	for key := range s.fetch.errors.last {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// RunBackgroundFetchPass performs one sorted, common-dir-deduplicated pass.
func (s *Service) RunBackgroundFetchPass(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.shuttingDown() || s.store == nil || s.backgroundFetchEnabled == nil || !s.backgroundFetchEnabled() {
		return
	}
	projects, err := s.store.ListProjects()
	if err != nil {
		if s.fetch.errors.shouldLog(backgroundFetchStoreKey, err.Error()) {
			s.log("git background fetch: list projects: %v", err)
		}
		return
	}
	s.fetch.errors.clear(backgroundFetchStoreKey)

	paths := make([]string, 0, len(projects))
	for _, project := range projects {
		if project.Path != "" {
			paths = append(paths, project.Path)
		}
	}
	slices.Sort(paths)
	live := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if ctx.Err() != nil || s.shuttingDown() {
			return
		}
		commonDir, err := s.core.CommonDir(path)
		if err != nil {
			key := backgroundFetchPathKey + path
			live[key] = struct{}{}
			if s.fetch.errors.shouldLog(key, err.Error()) {
				s.log("git background fetch: skipping %s: %v", path, err)
			}
			continue
		}
		key := backgroundFetchRepoKey + commonDir
		if _, seen := live[key]; seen {
			continue
		}
		live[key] = struct{}{}
		if _, err := s.core.FetchRemotesBackground(ctx, path); err != nil {
			if s.fetch.errors.shouldLog(key, err.Error()) {
				s.log("git background fetch: %s: %v", commonDir, err)
			}
			continue
		}
		s.fetch.errors.clear(key)
	}
	s.fetch.errors.retain(live)
}

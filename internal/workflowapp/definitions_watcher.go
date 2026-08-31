package workflowapp

import (
	"time"

	"agent-overflow/internal/workflowdefs"
)

const definitionsDebounce = 250 * time.Millisecond

func (s *Service) StartDefinitionsWatcher(root string, changed func()) {
	watcher, err := workflowdefs.NewWatcher(root, definitionsDebounce, changed)
	if err != nil {
		s.deps.Logf("workflow definitions watcher unavailable: %v", err)
		return
	}
	s.runtimeMu.Lock()
	s.definitionsWatcher = watcher
	s.runtimeMu.Unlock()
}

func (s *Service) CloseDefinitionsWatcher() error {
	s.runtimeMu.RLock()
	watcher := s.definitionsWatcher
	s.runtimeMu.RUnlock()
	if watcher == nil {
		return nil
	}
	return watcher.Close()
}

func (s *Service) HasDefinitionsWatcher() bool {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.definitionsWatcher != nil
}

package main

import "fmt"

type sessionStart struct {
	done chan struct{}
	err  error
}

func (a *App) runSessionStart(threadID string, start func() error) error {
	startState, leader := a.beginSessionStart(threadID)
	if !leader {
		<-startState.done
		return startState.err
	}

	startState.err = start()
	a.finishSessionStart(threadID, startState)
	return startState.err
}

func (a *App) beginSessionStart(threadID string) (*sessionStart, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.startingSessions == nil {
		a.startingSessions = make(map[string]*sessionStart)
	}
	if inFlight, ok := a.startingSessions[threadID]; ok {
		return inFlight, false
	}

	startState := &sessionStart{done: make(chan struct{})}
	a.startingSessions[threadID] = startState
	return startState, true
}

func (a *App) finishSessionStart(threadID string, startState *sessionStart) {
	a.mu.Lock()
	delete(a.startingSessions, threadID)
	a.mu.Unlock()
	close(startState.done)
}

func closeProviderSession(threadID string, sess session) error {
	switch {
	case sess.claude != nil:
		if err := sess.claude.Close(); err != nil {
			return fmt.Errorf("close claude session for thread %s: %w", threadID, err)
		}
	case sess.codex != nil:
		if err := sess.codex.Close(); err != nil {
			return fmt.Errorf("close codex session for thread %s: %w", threadID, err)
		}
	}

	return nil
}

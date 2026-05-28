package main

import (
	"time"

	"agent-overflow/internal/provider"
)

type sessionManager struct {
	app *App
}

func (a *App) sessionManager() sessionManager {
	return sessionManager{app: a}
}

func (m sessionManager) get(threadID string) (session, bool) {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()
	sess, ok := m.app.sessions[threadID]
	return sess, ok
}

func (m sessionManager) put(threadID string, sess session) {
	m.app.mu.Lock()
	m.app.sessions[threadID] = sess
	m.app.mu.Unlock()
}

func (m sessionManager) take(threadID string) (session, bool) {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()
	sess, ok := m.app.sessions[threadID]
	if ok {
		delete(m.app.sessions, threadID)
	}
	return sess, ok
}

func (m sessionManager) beginStart(threadID string) (*sessionStart, bool) {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()

	if m.app.startingSessions == nil {
		m.app.startingSessions = make(map[string]*sessionStart)
	}
	if inFlight, ok := m.app.startingSessions[threadID]; ok {
		return inFlight, false
	}

	startState := &sessionStart{done: make(chan struct{})}
	m.app.startingSessions[threadID] = startState
	return startState, true
}

func (m sessionManager) finishStart(threadID string, startState *sessionStart) {
	m.app.mu.Lock()
	delete(m.app.startingSessions, threadID)
	m.app.mu.Unlock()
	close(startState.done)
}

func (m sessionManager) startState(threadID string) (*sessionStart, bool) {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()
	startState, ok := m.app.startingSessions[threadID]
	return startState, ok
}

// unregister removes the session for threadID iff it still carries
// sessionToken (guarding against a newer session having replaced it), and
// returns the removed session so the caller can release its process group
// from the orphan reaper. Returns ok=false (and the zero session) when
// nothing matched.
func (m sessionManager) unregister(threadID, sessionToken string) (session, bool) {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()

	current, ok := m.app.sessions[threadID]
	if !ok || current.token != sessionToken {
		return session{}, false
	}
	delete(m.app.sessions, threadID)
	return current, true
}

func (m sessionManager) recordActivity(threadID, sessionToken string, kind provider.EventKind, content string, now time.Time) {
	current, ok := m.get(threadID)
	if !ok || current.token != sessionToken || current.liveness == nil {
		return
	}

	current.liveness.bumpActivity(now)
	switch kind {
	case provider.EventTurnStart:
		current.liveness.activeTurns.Add(1)
	case provider.EventTurnComplete:
		// Clamp to 0 so an unmatched TurnComplete (e.g. from a replayed
		// envelope) can't drive the counter negative.
		decrementActiveTurnsClamped(&current.liveness.activeTurns)
	case provider.EventSessionStatus:
		if content == "disconnected" {
			current.liveness.activeTurns.Store(0)
		}
	}
}

func (m sessionManager) hasProvider(providerName string) bool {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()
	for _, sess := range m.app.sessions {
		if sess.provider == providerName {
			return true
		}
	}
	return false
}

func (m sessionManager) idleCandidates(cutoffNano int64) []string {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()

	candidates := make([]string, 0, len(m.app.sessions))
	for threadID, sess := range m.app.sessions {
		if sess.liveness == nil {
			continue
		}
		if sess.liveness.activeTurns.Load() > 0 {
			continue
		}
		if sess.liveness.lastActivityUnixNano.Load() > cutoffNano {
			continue
		}
		candidates = append(candidates, threadID)
	}
	return candidates
}

func (m sessionManager) takeIdle(threadID string, cutoffNano int64) (session, bool) {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()

	sess, ok := m.app.sessions[threadID]
	if !ok {
		return session{}, false
	}
	if sess.liveness != nil {
		if sess.liveness.activeTurns.Load() > 0 {
			return session{}, false
		}
		if sess.liveness.lastActivityUnixNano.Load() > cutoffNano {
			return session{}, false
		}
	}
	delete(m.app.sessions, threadID)
	return sess, true
}

func (m sessionManager) snapshotAndClear() map[string]session {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()

	sessions := make(map[string]session, len(m.app.sessions))
	for threadID, sess := range m.app.sessions {
		sessions[threadID] = sess
	}
	m.app.sessions = make(map[string]session)
	return sessions
}

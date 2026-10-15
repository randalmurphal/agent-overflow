package app

import (
	"maps"
	"sync"

	"agent-overflow/internal/eventchan"
)

// SessionLiveEvent is the provider:session_live payload: whether a thread
// has a provider session live or starting. End Session reads it, so a
// menu never offers to end a session that is not there.
type SessionLiveEvent struct {
	ThreadID string `json:"threadId"`
	Live     bool   `json:"live"`
}

// appSessionLiveState holds every thread last published as having a
// session. The sends-pending worker publishes it, since the session
// registry marks that worker on every change (sessionManager.changed).
type appSessionLiveState struct {
	// mu is held across the read and the emit, so the set never runs
	// ahead of the frames and publishers emit in the order they read.
	mu   sync.Mutex
	live map[string]struct{}
}

// publishSessionLive reads whether threadID has a session and publishes it
// when it changed.
func (a *App) publishSessionLive(threadID string) {
	s := &a.sessionLive
	s.mu.Lock()
	defer s.mu.Unlock()
	live := a.sessionManager().liveOrStarting(threadID)
	if _, was := s.live[threadID]; was == live {
		return
	}
	if live {
		if s.live == nil {
			s.live = make(map[string]struct{})
		}
		s.live[threadID] = struct{}{}
	} else {
		delete(s.live, threadID)
	}
	a.emit(eventchan.ProviderSessionLive, SessionLiveEvent{ThreadID: threadID, Live: live})
}

// sessionLiveThreads returns every thread last published as having a
// session. A change not yet published is a frame still to come.
func (a *App) sessionLiveThreads() map[string]struct{} {
	s := &a.sessionLive
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.live)
}

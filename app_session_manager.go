package main

import (
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// sessionManager is the ONLY mutator of App.sessions, which is what lets the
// `ao` scoped-token registry (App.aoTokens) be maintained here rather than at
// every spawn/teardown call site: a token is registered exactly when its
// session enters the map and revoked exactly when it leaves, so no path can
// leave a live credential behind a dead process. See app_ao_session.go.
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
	if displaced, ok := m.app.sessions[threadID]; ok {
		// Defensive: every caller stops the prior session first, so this should
		// find nothing. If a future path ever replaces in place, the displaced
		// session's credential must not survive it.
		m.app.revokeAOTokenLocked(displaced)
	}
	m.app.sessions[threadID] = sess
	m.app.registerAOTokenLocked(sess)
	m.app.mu.Unlock()
}

func (m sessionManager) take(threadID string) (session, bool) {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()
	sess, ok := m.app.sessions[threadID]
	if ok {
		delete(m.app.sessions, threadID)
		m.app.revokeAOTokenLocked(sess)
		m.app.purgeClaudeLiveConfigStateLocked(sess.token)
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

// updateLaunchOpts replaces the stored launch options for threadID iff the
// session still carries sessionToken (guarding against a newer session
// having replaced the one whose config was just live-applied). Called by
// the config reconciler after a successful live apply so later reconciles
// diff against what the session is actually running.
func (m sessionManager) updateLaunchOpts(threadID, sessionToken string, opts provider.SessionOptions) {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()
	current, ok := m.app.sessions[threadID]
	if !ok || current.token != sessionToken {
		return
	}
	current.launchOpts = opts
	m.app.sessions[threadID] = current
}

// mutateLaunchOpts edits individual fields of the stored launch options
// under the same token guard as updateLaunchOpts, reporting whether the
// mutation applied (false: no session, or a newer session replaced the one
// the caller observed). Used by the async live-config confirmation path,
// which must touch exactly one axis without clobbering whatever a
// concurrent reconcile wrote to the others — and must skip its persistent
// side effects entirely when the session it is confirming is gone.
func (m sessionManager) mutateLaunchOpts(threadID, sessionToken string, mutate func(*provider.SessionOptions)) bool {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()
	current, ok := m.app.sessions[threadID]
	if !ok || current.token != sessionToken {
		return false
	}
	mutate(&current.launchOpts)
	m.app.sessions[threadID] = current
	return true
}

func (m sessionManager) updateCredentials(
	threadID, sessionToken string,
	generation uint64,
	accountID string,
	account provider.AccountInfo,
) {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()
	current, ok := m.app.sessions[threadID]
	if !ok || current.token != sessionToken || current.credentialGeneration > generation {
		return
	}
	current.credentialGeneration = generation
	current.credentialAccountID = accountID
	current.credentialAccount = account
	m.app.sessions[threadID] = current
}

// updateProviderCredentials applies a provider-wide hot credential switch to
// every matching live session and returns the affected thread IDs for event
// emission after releasing the session map lock. Claude is the only production
// caller: Codex app-servers retain their cached account until reconnect.
func (m sessionManager) updateProviderCredentials(
	providerName string,
	generation uint64,
	accountID string,
	account provider.AccountInfo,
) []string {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()

	threadIDs := make([]string, 0)
	for threadID, current := range m.app.sessions {
		if current.provider != providerName {
			continue
		}
		if current.credentialGeneration > generation {
			continue
		}
		current.credentialGeneration = generation
		current.credentialAccountID = accountID
		current.credentialAccount = account
		m.app.sessions[threadID] = current
		threadIDs = append(threadIDs, threadID)
	}
	return threadIDs
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
	m.app.revokeAOTokenLocked(current)
	m.app.purgeClaudeLiveConfigStateLocked(current.token)
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

// anyCodexSession returns some live Codex session and the account id its
// process authenticated as, or nil when none is running. "Some" is
// deliberate: the caller (account-level usage) asks a question about the
// LOGIN, not about a thread, and every session sharing that login answers it
// identically — so the account id is returned alongside so the caller can
// refuse a session whose process is still holding a superseded credential.
func (m sessionManager) anyCodexSession() (*codex.Session, string) {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()
	for _, sess := range m.app.sessions {
		if sess.codex != nil {
			return sess.codex, sess.credentialAccountID
		}
	}
	return nil, ""
}

// codexSessionForBinary returns some live Codex session whose process was
// spawned from binary, or nil when none is.
//
// The binary match is the caution this read needs, and it is a different
// one from anyCodexSession's account match. `skills/list` is answered out
// of the running binary's bundled skill set and its config schema, so a
// session still running an older codex than the setting now names would
// answer a question about a build the caller is not asking about. The
// account is deliberately NOT matched: skills resolve from the canonical
// CODEX_HOME plus the requested cwd, neither of which a login switch
// touches (see internal/codexskills.Key).
func (m sessionManager) codexSessionForBinary(binary string) *codex.Session {
	binary = normalizeCodexBinary(binary)
	m.app.mu.Lock()
	defer m.app.mu.Unlock()
	for _, sess := range m.app.sessions {
		if sess.codex != nil && normalizeCodexBinary(sess.codex.Binary()) == binary {
			return sess.codex
		}
	}
	return nil
}

// normalizeCodexBinary folds the unset binary setting onto the same value
// codex.NewSession defaults to, so "" and "codex" cannot look like two
// different builds.
func normalizeCodexBinary(binary string) string {
	binary = strings.TrimSpace(binary)
	if binary == "" {
		return "codex"
	}
	return binary
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

// threadIDsForProviderOrStarting snapshots the thread ids a settings-driven
// sweep must visit for one provider: every live session on it, PLUS every
// thread whose session start is still in flight.
//
// The starting half is not optional. A spawn snapshots Settings while it
// builds its session options; a save landing after that snapshot but before
// the session registers is invisible to a sweep that scans `sessions` alone,
// and nothing ever reconciles that thread again — it runs the pre-save
// config for the whole life of the session. Only a restart (or another
// settings change) would correct it.
//
// An in-flight start has no provider yet — the thread row is read inside the
// start — so starting threads are included regardless of providerName. That
// costs nothing: the per-thread reconcile waits for the start and then diffs
// the session that actually exists, which converges to a no-op for a thread
// that turned out to be on another provider.
//
// Both maps are read under ONE lock so the registration handoff cannot hide
// a thread: the start puts the session into `sessions` before runSessionStart
// clears `startingSessions`, so a starting thread is in one map or briefly in
// both — never in neither.
func (m sessionManager) threadIDsForProviderOrStarting(providerName string) []string {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()
	ids := make([]string, 0, len(m.app.sessions)+len(m.app.startingSessions))
	seen := make(map[string]struct{}, len(m.app.sessions)+len(m.app.startingSessions))
	for threadID, sess := range m.app.sessions {
		if sess.provider != providerName {
			continue
		}
		seen[threadID] = struct{}{}
		ids = append(ids, threadID)
	}
	for threadID := range m.app.startingSessions {
		if _, dup := seen[threadID]; dup {
			continue
		}
		ids = append(ids, threadID)
	}
	return ids
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
	m.app.revokeAOTokenLocked(sess)
	m.app.purgeClaudeLiveConfigStateLocked(sess.token)
	return sess, true
}

func (m sessionManager) snapshotAndClear() map[string]session {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()

	sessions := make(map[string]session, len(m.app.sessions))
	for threadID, sess := range m.app.sessions {
		sessions[threadID] = sess
		m.app.revokeAOTokenLocked(sess)
		m.app.purgeClaudeLiveConfigStateLocked(sess.token)
	}
	m.app.sessions = make(map[string]session)
	return sessions
}

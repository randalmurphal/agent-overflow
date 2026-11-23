package sessionruntime

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

// Start tracks one single-flighted provider start.
type Start struct {
	Done chan struct{}
	Err  error
	// Registered is the session the start registered (Put), kept for its
	// leader and joiners after the registry has moved on: a session that
	// fails before it initializes can be unregistered before Done closes.
	Registered Entry
}

type ClaudeMCPSession struct {
	ThreadID string
	Session  *claude.Session
}

type CodexMCPSession struct {
	ThreadID string
	Session  *codex.Session
}

// BrowserMCPSession is one live provider session that can receive the
// app-managed browser MCP server through its native live-refresh primitive.
type BrowserMCPSession struct {
	ThreadID string
	Claude   *claude.Session
	Codex    *codex.Session
}

// ThreadProcess is one live provider process root, named by the thread it
// serves. The dev-server scan (internal/devscan) traces a listening
// socket back to a thread through these: a dev server an agent started
// is a descendant of one of them, or a member of its process group.
type ThreadProcess struct {
	ThreadID string
	// PID is the provider subprocess (or owned child root).
	PID int
	// PGID is the process group id for daemonised descendants.
	PGID int
	// WorkDir is the thread's workspace directory, used as a tie-breaker
	// when multiple threads share a multiplexed provider connection.
	WorkDir string
}

// ClaudeLiveApply is one asynchronous /effort or /fast confirmation.
type ClaudeLiveApply struct {
	ThreadID        string
	SessionToken    string
	Axis            string
	Requested       string
	PreviousEffort  provider.ReasoningEffort
	PreviousFast    bool
	SentAt          time.Time
	Generation      uint64
	DeferredForTurn bool
	Defunct         bool
}

type liveApplyKey struct {
	sessionToken string
	axis         string
}

// Manager is the sole owner of provider-session runtime state. Its mutex is a
// leaf: methods never call into App, stores, providers, or callbacks while it
// is held.
type Manager struct {
	mu sync.Mutex

	sessions map[string]Entry
	// sealed is set by SealAndSnapshot, after which Put registers nothing.
	sealed                              bool
	starting                            map[string]*Start
	reconnecting                        map[string]bool
	autoReconnectAttempted              map[string]bool
	pendingConfigReconnects             map[string]bool
	configReconnectPollIntervalOverride time.Duration
	configReconnectQuietWindowOverride  time.Duration

	claudeLiveApplies                   map[string]ClaudeLiveApply
	claudeLiveApplyDegraded             map[liveApplyKey]struct{}
	claudeLiveApplyGenerations          map[liveApplyKey]uint64
	claudeLiveApplyConfirmAfterOverride time.Duration
	liveClaudeReconcileRunning          bool
	liveClaudeReconcileDirty            bool
	reconcileSessionConfigFn            func(string)
	readClaudeAppliedSettingsFn         func(string, string) (*claude.AppliedSettings, error)
	idleReaperStop                      chan struct{}
	idleReaperWG                        sync.WaitGroup
	retentionCleanupStop                chan struct{}
	retentionCleanupWG                  sync.WaitGroup
	connections                         *ConnPool
}

func New() *Manager {
	return &Manager{
		sessions:                   make(map[string]Entry),
		starting:                   make(map[string]*Start),
		reconnecting:               make(map[string]bool),
		autoReconnectAttempted:     make(map[string]bool),
		pendingConfigReconnects:    make(map[string]bool),
		claudeLiveApplies:          make(map[string]ClaudeLiveApply),
		claudeLiveApplyDegraded:    make(map[liveApplyKey]struct{}),
		claudeLiveApplyGenerations: make(map[liveApplyKey]uint64),
		connections:                NewConnPool(),
	}
}

// Connections returns the shared provider-connection pool owned by this manager.
func (m *Manager) Connections() *ConnPool {
	return m.connections
}

func (m *Manager) Get(threadID string) (Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[threadID]
	return entry, ok
}

func (m *Manager) ProviderSession(threadID string) (provider.Session, bool) {
	entry, ok := m.Get(threadID)
	if !ok {
		return nil, false
	}
	session := entry.ProviderSession()
	return session, session != nil
}

func (m *Manager) Claude(threadID string) (*claude.Session, bool) {
	entry, ok := m.Get(threadID)
	return entry.Claude, ok && entry.Claude != nil
}

func (m *Manager) Codex(threadID string) (*codex.Session, bool) {
	entry, ok := m.Get(threadID)
	return entry.Codex, ok && entry.Codex != nil
}

type AccountSnapshot struct {
	Provider             string
	SessionToken         string
	CredentialGeneration uint64
	CredentialAccountID  string
	CredentialAccount    provider.AccountInfo
}

func (m *Manager) Account(threadID string) (AccountSnapshot, bool) {
	entry, ok := m.Get(threadID)
	if !ok {
		return AccountSnapshot{}, false
	}
	return AccountSnapshot{
		Provider:             entry.Provider,
		SessionToken:         entry.Token,
		CredentialGeneration: entry.CredentialGeneration,
		CredentialAccountID:  entry.CredentialAccountID,
		CredentialAccount:    entry.CredentialAccount,
	}, true
}

// Put registers entry as the thread's live session and reports whether it
// did: once the registry is sealed it registers nothing, and the caller owns
// closing what it started.
func (m *Manager) Put(threadID string, entry Entry) bool {
	m.mu.Lock()
	if m.sealed {
		m.mu.Unlock()
		return false
	}
	if displaced, ok := m.sessions[threadID]; ok {
		m.purgeClaudeLiveConfigState(displaced.Token)
	}
	m.sessions[threadID] = entry
	if start := m.starting[threadID]; start != nil {
		start.Registered = entry
	}
	m.mu.Unlock()
	return true
}

func (m *Manager) Take(threadID string) (Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[threadID]
	if ok {
		m.remove(threadID, entry)
	}
	return entry, ok
}

func (m *Manager) Unregister(threadID, sessionToken string) (Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[threadID]
	if !ok || entry.Token != sessionToken {
		return Entry{}, false
	}
	m.remove(threadID, entry)
	return entry, true
}

func (m *Manager) remove(threadID string, entry Entry) {
	delete(m.sessions, threadID)
	m.purgeClaudeLiveConfigState(entry.Token)
}

func (m *Manager) BeginStart(threadID string) (*Start, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.starting[threadID]; ok {
		return current, false
	}
	start := &Start{Done: make(chan struct{})}
	m.starting[threadID] = start
	return start, true
}

func (m *Manager) FinishStart(threadID string, start *Start) {
	m.mu.Lock()
	if m.starting[threadID] == start {
		delete(m.starting, threadID)
	}
	m.mu.Unlock()
	close(start.Done)
}

func (m *Manager) StartState(threadID string) (*Start, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	start, ok := m.starting[threadID]
	return start, ok
}

func (m *Manager) ThreadIDsForProviderOrStarting(providerName string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.sessions)+len(m.starting))
	seen := make(map[string]struct{}, len(m.sessions)+len(m.starting))
	for threadID, entry := range m.sessions {
		if entry.Provider != providerName {
			continue
		}
		seen[threadID] = struct{}{}
		ids = append(ids, threadID)
	}
	for threadID := range m.starting {
		if _, exists := seen[threadID]; !exists {
			ids = append(ids, threadID)
		}
	}
	return ids
}

func (m *Manager) UpdateLaunchOptions(threadID, sessionToken string, options provider.SessionOptions) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[threadID]
	if !ok || entry.Token != sessionToken {
		return
	}
	entry.LaunchOptions = options
	m.sessions[threadID] = entry
}

func (m *Manager) UpdateReasoningEffort(threadID, sessionToken string, effort provider.ReasoningEffort) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[threadID]
	if !ok || entry.Token != sessionToken {
		return false
	}
	entry.LaunchOptions.ReasoningEffort = effort
	m.sessions[threadID] = entry
	return true
}

func (m *Manager) UpdateFastMode(threadID, sessionToken string, enabled bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[threadID]
	if !ok || entry.Token != sessionToken {
		return false
	}
	entry.LaunchOptions.FastMode = enabled
	m.sessions[threadID] = entry
	return true
}

func (m *Manager) UpdateCredentials(threadID, sessionToken string, generation uint64, accountID string, account provider.AccountInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[threadID]
	if !ok || entry.Token != sessionToken || entry.CredentialGeneration > generation {
		return
	}
	entry.CredentialGeneration = generation
	entry.CredentialAccountID = accountID
	entry.CredentialAccount = account
	m.sessions[threadID] = entry
}

func (m *Manager) UpdateProviderCredentials(providerName string, generation uint64, accountID string, account provider.AccountInfo) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var threadIDs []string
	for threadID, entry := range m.sessions {
		if entry.Provider != providerName || entry.CredentialGeneration > generation {
			continue
		}
		entry.CredentialGeneration = generation
		entry.CredentialAccountID = accountID
		entry.CredentialAccount = account
		m.sessions[threadID] = entry
		threadIDs = append(threadIDs, threadID)
	}
	return threadIDs
}

// RecordActivity stamps a provider event on the session it came from. It
// holds the lock idle removal holds (TakeIdle), so an event either lands
// before the removal's check and keeps its session, or finds it gone.
func (m *Manager) RecordActivity(threadID, sessionToken string, kind provider.EventKind, content string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[threadID]
	if !ok || entry.Token != sessionToken || entry.Liveness == nil {
		return
	}
	// Connection-wide broadcasts (such as account/rateLimits/updated) fan out
	// to every session on a shared process and must not reset idle timers on
	// quiet threads.
	if kind != provider.EventRateLimits {
		entry.Liveness.BumpActivity(now)
	}
	if kind == provider.EventTurnStart {
		entry.Liveness.ActiveTurns.Add(1)
	} else if kind == provider.EventTurnComplete {
		decrementActiveTurnsClamped(&entry.Liveness.ActiveTurns)
	} else if kind == provider.EventSessionStatus {
		if content == "disconnected" {
			entry.Liveness.ActiveTurns.Store(0)
		}
	}
}

func (m *Manager) AnyCodexSession() (*codex.Session, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.sessions {
		if entry.Codex != nil {
			return entry.Codex, entry.CredentialAccountID
		}
	}
	return nil, ""
}

func normalizeCodexBinary(binary string) string {
	if binary = strings.TrimSpace(binary); binary == "" {
		return "codex"
	}
	return binary
}

func (m *Manager) CodexSessionForBinary(binary string) *codex.Session {
	binary = normalizeCodexBinary(binary)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.sessions {
		if entry.Codex != nil && normalizeCodexBinary(entry.Codex.Binary()) == binary {
			return entry.Codex
		}
	}
	return nil
}

func (m *Manager) CodexMCPSessions() []CodexMCPSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]CodexMCPSession, 0)
	for threadID, entry := range m.sessions {
		if entry.Codex != nil {
			result = append(result, CodexMCPSession{ThreadID: threadID, Session: entry.Codex})
		}
	}
	return result
}

func (m *Manager) ClaudeMCPSessions(workspacePath string) []ClaudeMCPSession {
	workspacePath = filepath.Clean(workspacePath)
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]ClaudeMCPSession, 0)
	for threadID, entry := range m.sessions {
		if entry.Claude != nil && filepath.Clean(entry.LaunchOptions.WorkDir) == workspacePath {
			result = append(result, ClaudeMCPSession{ThreadID: threadID, Session: entry.Claude})
		}
	}
	return result
}

func (m *Manager) BrowserMCPSessions() []BrowserMCPSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]BrowserMCPSession, 0, len(m.sessions))
	for threadID, entry := range m.sessions {
		if entry.Claude == nil && entry.Codex == nil {
			continue
		}
		result = append(result, BrowserMCPSession{
			ThreadID: threadID,
			Claude:   entry.Claude,
			Codex:    entry.Codex,
		})
	}
	return result
}

// ThreadProcesses returns the live provider process roots of every registered
// thread. Sessions are snapshotted under the manager's lock and process roots
// are read after it is released so the manager mutex and a session mutex are
// never held together.
func (m *Manager) ThreadProcesses() []ThreadProcess {
	type live struct {
		threadID string
		workDir  string
		session  provider.Session
	}
	m.mu.Lock()
	sessions := make([]live, 0, len(m.sessions))
	for threadID, entry := range m.sessions {
		if session := entry.ProviderSession(); session != nil {
			sessions = append(sessions, live{
				threadID: threadID,
				workDir:  entry.LaunchOptions.WorkDir,
				session:  session,
			})
		}
	}
	m.mu.Unlock()

	processes := make([]ThreadProcess, 0, len(sessions))
	for _, entry := range sessions {
		owned := entry.session.OwnedProcesses()
		for _, root := range owned {
			if root.PID <= 0 {
				continue
			}
			pgid := root.PGID
			if pgid <= 0 {
				pgid = root.PID
			}
			workDir := root.WorkDir
			if workDir == "" {
				workDir = entry.workDir
			}
			processes = append(processes, ThreadProcess{
				ThreadID: entry.threadID,
				PID:      root.PID,
				PGID:     pgid,
				WorkDir:  workDir,
			})
		}
	}
	return processes
}

func (m *Manager) IdleCandidates(cutoffNano int64) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []string
	for threadID, entry := range m.sessions {
		if entry.Liveness != nil && entry.Liveness.ActiveTurns.Load() == 0 && entry.Liveness.LastActivityUnixNano.Load() <= cutoffNano {
			result = append(result, threadID)
		}
	}
	return result
}

func (m *Manager) TakeIdle(threadID string, cutoffNano int64) (Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[threadID]
	if !ok {
		return Entry{}, false
	}
	if entry.Liveness != nil && (entry.Liveness.ActiveTurns.Load() > 0 || entry.Liveness.LastActivityUnixNano.Load() > cutoffNano) {
		return Entry{}, false
	}
	m.remove(threadID, entry)
	return entry, true
}

// SealAndSnapshot takes every registered session out for the caller to close
// and seals the registry: a later Put registers nothing, so a start that
// finishes after shutdown's session close cannot leave a session nobody
// closes. Only the end of the App's life calls it; a harness reset does not.
func (m *Manager) SealAndSnapshot() map[string]Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sealed = true
	result := make(map[string]Entry, len(m.sessions))
	for threadID, entry := range m.sessions {
		result[threadID] = entry
		m.purgeClaudeLiveConfigState(entry.Token)
	}
	m.sessions = make(map[string]Entry)
	return result
}

func (m *Manager) Snapshot() map[string]Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]Entry, len(m.sessions))
	for threadID, entry := range m.sessions {
		result[threadID] = entry
	}
	return result
}

func (m *Manager) BumpAllActivity(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.sessions {
		entry.Liveness.BumpActivity(now)
	}
}

func (m *Manager) BeginReconnect(threadID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reconnecting[threadID] {
		return false
	}
	m.reconnecting[threadID] = true
	return true
}

func (m *Manager) FinishReconnect(threadID string) {
	m.mu.Lock()
	delete(m.reconnecting, threadID)
	m.mu.Unlock()
}

func (m *Manager) BeginAutoReconnect(threadID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.autoReconnectAttempted[threadID] {
		return false
	}
	m.autoReconnectAttempted[threadID] = true
	return true
}

func (m *Manager) ClearAutoReconnect(threadID string) {
	m.mu.Lock()
	delete(m.autoReconnectAttempted, threadID)
	m.mu.Unlock()
}

func (m *Manager) BeginPendingConfigReconnect(threadID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingConfigReconnects[threadID] {
		return false
	}
	m.pendingConfigReconnects[threadID] = true
	return true
}

func (m *Manager) ClearPendingConfigReconnect(threadID string) {
	m.mu.Lock()
	delete(m.pendingConfigReconnects, threadID)
	m.mu.Unlock()
}

func (m *Manager) PendingConfigReconnect(threadID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pendingConfigReconnects[threadID]
}

func (m *Manager) ConfigReconnectOverrides() (time.Duration, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.configReconnectPollIntervalOverride, m.configReconnectQuietWindowOverride
}

func (m *Manager) SetConfigReconnectOverrides(poll, quiet time.Duration) {
	m.mu.Lock()
	m.configReconnectPollIntervalOverride = poll
	m.configReconnectQuietWindowOverride = quiet
	m.mu.Unlock()
}

func (m *Manager) SetConfigReconnectPollOverride(poll time.Duration) {
	m.mu.Lock()
	m.configReconnectPollIntervalOverride = poll
	m.mu.Unlock()
}

func (m *Manager) SetConfigReconnectQuietOverride(quiet time.Duration) {
	m.mu.Lock()
	m.configReconnectQuietWindowOverride = quiet
	m.mu.Unlock()
}

func (m *Manager) StartIdleReaper() (chan struct{}, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idleReaperStop != nil {
		return nil, false
	}
	stop := make(chan struct{})
	m.idleReaperStop = stop
	m.idleReaperWG.Add(1)
	return stop, true
}

func (m *Manager) IdleReaperDone() { m.idleReaperWG.Done() }

func (m *Manager) StopIdleReaper() {
	m.mu.Lock()
	stop := m.idleReaperStop
	m.idleReaperStop = nil
	m.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	m.idleReaperWG.Wait()
}

func (m *Manager) StartRetentionCleanup() (chan struct{}, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.retentionCleanupStop != nil {
		return nil, false
	}
	stop := make(chan struct{})
	m.retentionCleanupStop = stop
	m.retentionCleanupWG.Add(1)
	return stop, true
}

func (m *Manager) RetentionCleanupDone() { m.retentionCleanupWG.Done() }

func (m *Manager) StopRetentionCleanup() {
	m.mu.Lock()
	stop := m.retentionCleanupStop
	m.retentionCleanupStop = nil
	m.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	m.retentionCleanupWG.Wait()
}

func (m *Manager) HasActiveTurn() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.sessions {
		if entry.Liveness != nil && entry.Liveness.ActiveTurns.Load() > 0 {
			return true
		}
	}
	return false
}

// HasActiveTurnOrRecent reports whether any provider is in an explicitly
// tracked turn or has emitted wire activity newer than cutoffNano.
func (m *Manager) HasActiveTurnOrRecent(cutoffNano int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.sessions {
		if entry.Liveness == nil {
			continue
		}
		if entry.Liveness.ActiveTurns.Load() > 0 || entry.Liveness.LastActivityUnixNano.Load() > cutoffNano {
			return true
		}
	}
	return false
}

func (m *Manager) purgeClaudeLiveConfigState(sessionToken string) {
	for id, apply := range m.claudeLiveApplies {
		if apply.SessionToken == sessionToken {
			delete(m.claudeLiveApplies, id)
		}
	}
	for key := range m.claudeLiveApplyDegraded {
		if key.sessionToken == sessionToken {
			delete(m.claudeLiveApplyDegraded, key)
		}
	}
	for key := range m.claudeLiveApplyGenerations {
		if key.sessionToken == sessionToken {
			delete(m.claudeLiveApplyGenerations, key)
		}
	}
}

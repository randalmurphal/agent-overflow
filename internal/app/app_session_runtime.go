package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/sessionruntime"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// sessionManager is a root naming adapter over the package-owned runtime. It
// holds no state; sessionruntime.Manager is the sole authority. Every change
// to whether a thread has a session, live or starting, is reported to
// changed: whether its queued messages count as pending depends on it
// (readSendsPending).
type sessionManager struct {
	runtime *sessionruntime.Manager
	changed func(threadID string)
}

func (a *App) sessionManager() sessionManager {
	a.sessionRuntimeOnce.Do(func() { a.sessionRuntime = sessionruntime.New() })
	return sessionManager{runtime: a.sessionRuntime, changed: a.markSendsPendingDirty}
}

func (m sessionManager) get(threadID string) (session, bool) { return m.runtime.Get(threadID) }
func (m sessionManager) put(threadID string, entry session) {
	m.runtime.Put(threadID, entry)
	m.changed(threadID)
}
func (m sessionManager) take(threadID string) (session, bool) {
	entry, ok := m.runtime.Take(threadID)
	m.changed(threadID)
	return entry, ok
}
func (m sessionManager) unregister(threadID, token string) (session, bool) {
	entry, ok := m.runtime.Unregister(threadID, token)
	m.changed(threadID)
	return entry, ok
}
func (m sessionManager) beginStart(threadID string) (*sessionStart, bool) {
	start, leader := m.runtime.BeginStart(threadID)
	m.changed(threadID)
	return start, leader
}
func (m sessionManager) finishStart(threadID string, start *sessionStart) {
	m.runtime.FinishStart(threadID, start)
	m.changed(threadID)
}
func (m sessionManager) startState(threadID string) (*sessionStart, bool) {
	return m.runtime.StartState(threadID)
}

// liveOrStarting reports whether a thread has a session registered or one
// starting.
func (m sessionManager) liveOrStarting(threadID string) bool {
	if _, ok := m.runtime.Get(threadID); ok {
		return true
	}
	_, starting := m.runtime.StartState(threadID)
	return starting
}
func (m sessionManager) updateLaunchOpts(threadID, token string, options provider.SessionOptions) {
	m.runtime.UpdateLaunchOptions(threadID, token, options)
}
func (m sessionManager) updateCredentials(threadID, token string, generation uint64, accountID string, account provider.AccountInfo) {
	m.runtime.UpdateCredentials(threadID, token, generation, accountID, account)
}
func (m sessionManager) updateProviderCredentials(providerName string, generation uint64, accountID string, account provider.AccountInfo) []string {
	return m.runtime.UpdateProviderCredentials(providerName, generation, accountID, account)
}
func (m sessionManager) anyCodexSession() (*codex.Session, string) {
	return m.runtime.AnyCodexSession()
}
func (m sessionManager) codexSessionForBinary(binary string) *codex.Session {
	return m.runtime.CodexSessionForBinary(binary)
}

func (m sessionManager) browserMCPSessions() []sessionruntime.BrowserMCPSession {
	return m.runtime.BrowserMCPSessions()
}

func (m sessionManager) threadIDsForProviderOrStarting(providerName string) []string {
	return m.runtime.ThreadIDsForProviderOrStarting(providerName)
}
func (m sessionManager) idleCandidates(cutoffNano int64) []string {
	return m.runtime.IdleCandidates(cutoffNano)
}
func (m sessionManager) takeIdle(threadID string, cutoffNano int64) (session, bool) {
	entry, ok := m.runtime.TakeIdle(threadID, cutoffNano)
	if ok {
		m.changed(threadID)
	}
	return entry, ok
}
func (m sessionManager) snapshot() map[string]session { return m.runtime.Snapshot() }
func (m sessionManager) snapshotAndClear() map[string]session {
	cleared := m.runtime.SnapshotAndClear()
	for threadID := range cleared {
		m.changed(threadID)
	}
	return cleared
}

type sessionStart = sessionruntime.Start

// runRegisteredSessionStart single-flights session starts per thread: the
// first caller leads and runs start(), everybody else joins its result, and
// each of them gets back the session the start registered: a session that
// failed before it initialized may already have left the registry.
//
// Two properties are load-bearing and were both learned the hard way:
//
//   - The leader's teardown is DEFERRED. A panic inside start() would
//     otherwise leave the in-flight entry registered and its `done` channel
//     open forever, so every later start of that thread — and every joiner
//     already parked — blocks for the life of the process.
//   - A joiner's wait is cancellable. It has performed no side effects, so
//     abandoning it costs nothing; the leader is unaffected and its start
//     runs to completion either way. (The leader's own wait is the provider
//     IO inside start(), which deliberately stays uncancellable — a
//     cancelled spawn/send is indistinguishable from a delivered one.)
func (a *App) runRegisteredSessionStart(ctx context.Context, threadID string, start func() error) (session, error) {
	startState, leader := a.sessionManager().beginStart(threadID)
	if !leader {
		select {
		case <-startState.Done:
			return startState.Registered, startState.Err
		case <-ctx.Done():
			return session{}, fmt.Errorf("waiting for in-flight session start of thread %s: %w", threadID, ctx.Err())
		}
	}

	completed := false
	defer func() {
		if !completed && startState.Err == nil {
			// The leader is unwinding through a panic. Joiners are released
			// with a real cause instead of a nil error plus a thread that
			// mysteriously has no session.
			startState.Err = fmt.Errorf("session start for thread %s panicked", threadID)
		}
		a.sessionManager().finishStart(threadID, startState)
	}()
	startState.Err = start()
	completed = true
	return startState.Registered, startState.Err
}

// revokeSessionMCP retires the AO tool URLs one provider session was issued.
// Each server matches the session, so a late teardown of an old session
// leaves its replacement's URLs working.
func (a *App) revokeSessionMCP(threadID, token string) {
	a.revokeRemoteMCP(threadID, token)
	a.revokeThreadMCP(threadID, token)
	a.workflowMCPServer().RevokeThread(threadID, token)
	a.trackerMCPServer().RevokeThread(threadID, token)
	a.trackerMCP.tools.EndSession(token)
	if a.browser.mcp != nil {
		a.browser.mcp.RevokeThread(threadID, token)
	}
}

func (a *App) closeProviderSession(threadID string, sess session) error {
	a.revokeSessionMCP(threadID, sess.Token)
	providerSess := sess.ProviderSession()
	if providerSess == nil {
		return nil
	}
	closeErr := providerSess.Close()
	// Close returns once the read loop is gone, which leaves its final
	// events (the session's own "disconnected" among them) queued rather
	// than handled. Callers rely on those being handled when this returns;
	// any a stalled worker still holds are refused once a replacement
	// start commits the thread to its own session.
	a.drainProviderEvents(threadID, "close provider session")
	a.reportClaudeLostWrites(threadID, sess)
	a.settlePendingWorktreeExit(threadID, sess.Token, false)
	if closeErr != nil {
		return fmt.Errorf("close %s session for thread %s: %w", sess.Provider, threadID, closeErr)
	}
	return nil
}

// reportClaudeLostWrites puts a Claude `system/session_writes_lost` on the
// thread as an error row once the process has ended. The CLI sends it only
// as it exits, which is usually because AO closed it after marking the
// thread stopped, so the wire path cannot carry it. Both ends of a session
// call this: closeProviderSession after Close, and unregisterSession after
// a self-exit's final event. The session hands the text out once, so a
// path that reaches both reports it once.
func (a *App) reportClaudeLostWrites(threadID string, sess session) {
	if sess.Claude == nil {
		return
	}
	if text := sess.Claude.TakeLostWritesError(); text != "" {
		a.emitErrorToThread(threadID, text)
	}
}

// deriveCallerScope answers "what authority does this thread's session have".
// A workflow phase or fan-out unit thread carries its frozen phase's grants and
// its own frozen access (derivePhaseScope). Every other app-owned session in a
// project is interactive: the user's authority over that project's runs. The
// threads of an assistant, which live outside every project, are interactive
// with no project: they drive threads and remote jobs, and have no project to
// run workflows in (workflowCallerScope).
func (a *App) deriveCallerScope(thread store.Thread) (transport.CallerScope, bool, error) {
	if a.store == nil {
		return transport.CallerScope{}, false, nil
	}
	if thread.Mode == threadmode.ModeWorkflow {
		return a.derivePhaseScope(thread)
	}
	if strings.TrimSpace(thread.ProjectID) == "" && thread.OwnerAgentID() == "" {
		// Any other projectless thread has nothing to scope against.
		return transport.CallerScope{}, false, nil
	}
	return transport.CallerScope{
		Kind: transport.ScopeKindInteractive, ThreadID: thread.ID, ProjectID: thread.ProjectID,
	}, true, nil
}

// workflowCallerScope is deriveCallerScope for the workflow tools: a thread
// with no project has no runs to start or list, and gets none.
func (a *App) workflowCallerScope(thread store.Thread) (transport.CallerScope, bool, error) {
	scope, ok, err := a.deriveCallerScope(thread)
	if err != nil || !ok || strings.TrimSpace(scope.ProjectID) == "" {
		return transport.CallerScope{}, false, err
	}
	return scope, true, nil
}

// sessionScope is a thread's workflowCallerScope, resolved on its first use
// and then kept. A session start, or a listing of the thread's MCP menu,
// makes one and hands it to every server that decides by it, so the
// resolution, which decodes a workflow phase's run snapshot, runs at most
// once and only for a server that needs it.
type sessionScope func() (transport.CallerScope, bool, error)

func (a *App) sessionScopeOf(thread store.Thread) sessionScope {
	var (
		once  sync.Once
		scope transport.CallerScope
		ok    bool
		err   error
	)
	return func() (transport.CallerScope, bool, error) {
		once.Do(func() { scope, ok, err = a.workflowCallerScope(thread) })
		return scope, ok, err
	}
}

// derivePhaseScope resolves the run, phase, frozen grants and access behind a
// workflow thread. A unit thread resolves through its unit row; a phase thread
// through its phase row. Either way both come from the run's SNAPSHOT, never
// from the definition on disk: a definition edited mid-run must not widen what
// an already-running phase may do.
func (a *App) derivePhaseScope(thread store.Thread) (transport.CallerScope, bool, error) {
	attached, err := a.workflowPhaseForThread(thread.ID)
	if err != nil {
		return transport.CallerScope{}, false, err
	}
	if attached.itemID == "" {
		// A workflow thread whose attempt row is not attached yet (or was torn
		// down) gets no credential rather than an unscoped one.
		return transport.CallerScope{}, false, nil
	}
	item, err := a.store.GetWorkItem(attached.itemID)
	if err != nil {
		return transport.CallerScope{}, false, fmt.Errorf("ao session scope for thread %s: %w", thread.ID, err)
	}
	authority, err := frozenPhaseAuthority(item.Snapshot, attached)
	if err != nil {
		return transport.CallerScope{}, false, fmt.Errorf("ao session scope for thread %s: %w", thread.ID, err)
	}
	return transport.CallerScope{
		Kind: transport.ScopeKindPhase, ThreadID: thread.ID, ProjectID: item.ProjectID,
		ItemID: item.ID, PhaseID: attached.phaseID, Grants: authority.grants, Access: string(authority.access),
	}, true, nil
}

// phaseAttachment is the work a workflow thread runs: a phase attempt, or one
// fan-out unit (or the join) of it.
type phaseAttachment struct {
	itemID, phaseID string
	unitID          string
	join            bool
}

// workflowPhaseForThread maps a workflow thread onto the (item, phase, unit) it
// is running. Unit rows are consulted first: a unit thread also has no phase
// row of its own, so asking the phase table first would attribute it to
// whichever phase attempt last touched the thread.
func (a *App) workflowPhaseForThread(threadID string) (phaseAttachment, error) {
	unit, found, err := a.store.GetWorkItemUnitByThread(threadID)
	if err != nil {
		return phaseAttachment{}, err
	}
	if found {
		return phaseAttachment{
			itemID: unit.ItemID, phaseID: unit.PhaseID,
			unitID: unit.UnitID, join: unit.Kind == store.WorkItemUnitKindJoin,
		}, nil
	}
	phase, found, err := a.store.GetWorkItemPhaseByThread(threadID)
	if err != nil {
		return phaseAttachment{}, err
	}
	if !found {
		return phaseAttachment{}, nil
	}
	return phaseAttachment{itemID: phase.ItemID, phaseID: phase.PhaseID}, nil
}

// phaseAuthority is what a phase session may do: its phase's grants (a unit
// inherits them, because a unit is that phase's work) and the access its own
// definition declares (a unit declares its own).
type phaseAuthority struct {
	grants []string
	access def.Access
}

// frozenPhaseAuthority reads a phase session's authority out of a run's frozen
// snapshot. A phase or unit the snapshot does not name is a wiring bug, not a
// permission question, so it is reported rather than resolved to "no grants".
// An empty snapshot is the pre-freeze state: no grants, default access.
func frozenPhaseAuthority(snapshot json.RawMessage, attached phaseAttachment) (phaseAuthority, error) {
	if len(snapshot) == 0 {
		return phaseAuthority{access: def.DefaultAccess}, nil
	}
	var decoded engine.Snapshot
	if err := json.Unmarshal(snapshot, &decoded); err != nil {
		return phaseAuthority{}, fmt.Errorf("decode run snapshot: %w", err)
	}
	for _, phase := range decoded.Workflow.Phases {
		if phase.ID != attached.phaseID {
			continue
		}
		authority := phaseAuthority{grants: make([]string, 0, len(phase.Grants)), access: phase.EffectiveAccess()}
		for _, grant := range phase.Grants {
			// A snapshot frozen before a grant was retired (or written by a
			// definition that failed validation on another path) must not hand
			// out authority this build cannot enforce.
			if def.KnownGrant(grant) {
				authority.grants = append(authority.grants, grant)
				continue
			}
			log.Printf("ao session scope: phase %q declares unknown grant %q; ignoring", attached.phaseID, grant)
		}
		if attached.unitID != "" {
			unit, found := def.UnitDefinition(phase, attached.unitID, attached.join)
			if !found {
				return phaseAuthority{}, fmt.Errorf("unit %q is not in phase %q of the run's frozen workflow", attached.unitID, attached.phaseID)
			}
			authority.access = unit.EffectiveAccess()
		}
		return authority, nil
	}
	return phaseAuthority{}, fmt.Errorf("phase %q is not in the run's frozen workflow", attached.phaseID)
}

package sessionruntime

import (
	"time"

	"agent-overflow/internal/provider/claude"
)

// RegisterClaudeLiveApplies atomically supersedes older applies for each axis,
// advances their generations, and installs the new command receipts.
func (m *Manager) RegisterClaudeLiveApplies(applies map[string]ClaudeLiveApply, now time.Time, staleAfter time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, pending := range m.claudeLiveApplies {
		if now.Sub(pending.SentAt) > staleAfter {
			delete(m.claudeLiveApplies, id)
		}
	}
	for id, apply := range applies {
		for existingID, pending := range m.claudeLiveApplies {
			if pending.SessionToken == apply.SessionToken && pending.Axis == apply.Axis && !pending.Defunct {
				pending.Defunct = true
				m.claudeLiveApplies[existingID] = pending
			}
		}
		key := liveApplyKey{sessionToken: apply.SessionToken, axis: apply.Axis}
		m.claudeLiveApplyGenerations[key]++
		apply.Generation = m.claudeLiveApplyGenerations[key]
		m.claudeLiveApplies[id] = apply
	}
}

func (m *Manager) ClaudeLiveApplySuperseded(apply ClaudeLiveApply) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claudeLiveApplyGenerations[liveApplyKey{sessionToken: apply.SessionToken, axis: apply.Axis}] > apply.Generation
}

func (m *Manager) RollbackClaudeLiveApplies(commandUUIDs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range commandUUIDs {
		if pending, ok := m.claudeLiveApplies[id]; ok {
			pending.Defunct = true
			m.claudeLiveApplies[id] = pending
		}
	}
}

func (m *Manager) PeekClaudeLiveApply(commandUUID string) (ClaudeLiveApply, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	apply, ok := m.claudeLiveApplies[commandUUID]
	return apply, ok
}

func (m *Manager) TakeClaudeLiveApply(commandUUID string) (ClaudeLiveApply, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	apply, ok := m.claudeLiveApplies[commandUUID]
	if ok {
		delete(m.claudeLiveApplies, commandUUID)
	}
	return apply, ok
}

func (m *Manager) NoteClaudeLiveApplyDeferredForTurn(commandUUID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	apply, ok := m.claudeLiveApplies[commandUUID]
	if !ok || apply.Defunct {
		return
	}
	apply.DeferredForTurn = true
	m.claudeLiveApplies[commandUUID] = apply
}

func (m *Manager) TakeClaudeLiveApplyTurnDeferral(commandUUID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	apply, ok := m.claudeLiveApplies[commandUUID]
	if !ok || apply.Defunct || !apply.DeferredForTurn {
		return false
	}
	apply.DeferredForTurn = false
	m.claudeLiveApplies[commandUUID] = apply
	return true
}

func (m *Manager) TakeClaudeLiveAppliesForAxis(sessionToken, axis string) []ClaudeLiveApply {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []ClaudeLiveApply
	for id, apply := range m.claudeLiveApplies {
		if apply.SessionToken == sessionToken && apply.Axis == axis {
			if !apply.Defunct {
				result = append(result, apply)
			}
			delete(m.claudeLiveApplies, id)
		}
	}
	return result
}

func (m *Manager) ClaudeLiveApplyIsDegraded(sessionToken string, update claude.LiveUpdate, effortAxis, fastAxis string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if update.Effort != "" {
		if _, ok := m.claudeLiveApplyDegraded[liveApplyKey{sessionToken: sessionToken, axis: effortAxis}]; ok {
			return true
		}
	}
	if update.FastMode != "" {
		if _, ok := m.claudeLiveApplyDegraded[liveApplyKey{sessionToken: sessionToken, axis: fastAxis}]; ok {
			return true
		}
	}
	return false
}

func (m *Manager) MarkClaudeLiveApplyDegraded(sessionToken, axis string) {
	m.mu.Lock()
	m.claudeLiveApplyDegraded[liveApplyKey{sessionToken: sessionToken, axis: axis}] = struct{}{}
	m.mu.Unlock()
}

func (m *Manager) ClaudeLiveApplyConfirmAfterOverride() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claudeLiveApplyConfirmAfterOverride
}

func (m *Manager) SetClaudeLiveApplyConfirmAfterOverride(value time.Duration) {
	m.mu.Lock()
	m.claudeLiveApplyConfirmAfterOverride = value
	m.mu.Unlock()
}

// BeginLiveClaudeReconcile starts a coalesced sweep. False means a sweep was
// already running and has been marked dirty.
func (m *Manager) BeginLiveClaudeReconcile() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.liveClaudeReconcileRunning {
		m.liveClaudeReconcileDirty = true
		return false
	}
	m.liveClaudeReconcileRunning = true
	m.liveClaudeReconcileDirty = false
	return true
}

// FinishLiveClaudeReconcileSweep reports whether the caller should immediately
// run one coalesced follow-up sweep.
func (m *Manager) FinishLiveClaudeReconcileSweep(shuttingDown bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.liveClaudeReconcileDirty && !shuttingDown {
		m.liveClaudeReconcileDirty = false
		return true
	}
	m.liveClaudeReconcileRunning = false
	m.liveClaudeReconcileDirty = false
	return false
}

func (m *Manager) LiveClaudeReconcileState() (running, dirty bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.liveClaudeReconcileRunning, m.liveClaudeReconcileDirty
}

func (m *Manager) ReconcileSessionConfigStep() func(string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reconcileSessionConfigFn
}

func (m *Manager) SetReconcileSessionConfigStep(step func(string)) {
	m.mu.Lock()
	m.reconcileSessionConfigFn = step
	m.mu.Unlock()
}

func (m *Manager) ReadClaudeAppliedSettingsStep() func(string, string) (*claude.AppliedSettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readClaudeAppliedSettingsFn
}

func (m *Manager) SetReadClaudeAppliedSettingsStep(step func(string, string) (*claude.AppliedSettings, error)) {
	m.mu.Lock()
	m.readClaudeAppliedSettingsFn = step
	m.mu.Unlock()
}

package main

import (
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// sessionAffectingFields enumerates the thread columns that, when
// changed, require a provider-session restart to take effect. The
// centralized restartSessionIfAffected helper consults this list so
// every per-field binding participates in the same restart policy.
var sessionAffectingFields = map[string]struct{}{
	"provider":      {},
	"model":         {},
	"mode":          {},
	"effort":        {},
	"fastMode":      {},
	"contextWindow": {},
	"runtimeMode":   {},
	"workspace":     {},
}

// restartSessionIfAffected emits the refreshed thread and, when the
// named field affects provider launch config AND a session is live,
// fires a best-effort session restart in the background. Centralizing
// the restart call keeps the per-field bindings free of duplicated
// "is this session live" plumbing.
//
// Returns the refreshed thread and any GetThread error. Restart
// failures are surfaced via a thread-scoped error event; we do NOT
// propagate them synchronously so the UI can still render the
// optimistic state.
func (a *App) restartSessionIfAffected(threadID, changedField string) (store.Thread, error) {
	refreshed, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	if _, ok := sessionAffectingFields[changedField]; !ok {
		return refreshed, nil
	}
	if !a.hasActiveSession(threadID) {
		return refreshed, nil
	}
	go func() {
		if err := a.ReconnectSession(threadID); err != nil {
			log.Printf("thread %s: %s change reconnect failed: %v", threadID, changedField, err)
			a.emitErrorToThread(threadID, fmt.Sprintf("%s change failed to reconnect: %v", changedField, err))
		}
	}()
	return refreshed, nil
}

// UpdateThreadProvider persists the provider column and restarts the
// session if one is live so the new provider takes effect.
func (a *App) UpdateThreadProvider(id, providerName string) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update provider: store unavailable")
	}
	normalized := strings.TrimSpace(providerName)
	if err := a.store.UpdateProvider(id, normalized); err != nil {
		return store.Thread{}, err
	}
	return a.restartSessionIfAffected(id, "provider")
}

// UpdateThreadReasoningEffort persists the effort tier and restarts the
// session if one is live.
func (a *App) UpdateThreadReasoningEffort(id, effort string) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update effort: store unavailable")
	}
	if err := a.store.UpdateReasoningEffort(id, effort); err != nil {
		return store.Thread{}, err
	}
	return a.restartSessionIfAffected(id, "effort")
}

// UpdateThreadFastMode persists the fast-mode boolean and restarts the
// session if one is live. Fast mode typically swaps the model to the
// provider's small-model tier (per the per-provider translator) so a
// running session won't pick up the change without a restart.
func (a *App) UpdateThreadFastMode(id string, on bool) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update fast mode: store unavailable")
	}
	if err := a.store.UpdateFastMode(id, on); err != nil {
		return store.Thread{}, err
	}
	return a.restartSessionIfAffected(id, "fastMode")
}

// UpdateThreadContextWindow persists the context window size and
// restarts the session if one is live.
func (a *App) UpdateThreadContextWindow(id string, tokens int) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update context window: store unavailable")
	}
	if err := a.store.UpdateContextWindow(id, tokens); err != nil {
		return store.Thread{}, err
	}
	return a.restartSessionIfAffected(id, "contextWindow")
}

// UpdateThreadRuntimeMode persists the runtime mode and restarts the
// session if one is live. Replaces the older SetThreadRuntimeMode
// naming; kept as a single surface so the frontend speaks one shape.
func (a *App) UpdateThreadRuntimeMode(id, mode string) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update runtime mode: store unavailable")
	}
	normalized := provider.RuntimeMode(mode)
	switch normalized {
	case provider.RuntimeApprovalRequired, provider.RuntimeAutoAcceptEdits, provider.RuntimeFullAccess:
		// ok
	default:
		return store.Thread{}, fmt.Errorf("update runtime mode: invalid mode %q", mode)
	}
	if err := a.store.UpdateRuntimeMode(id, string(normalized)); err != nil {
		return store.Thread{}, err
	}
	return a.restartSessionIfAffected(id, "runtimeMode")
}

// UpdateThreadBranch persists the branch column. Does NOT perform the
// git checkout — that flow lives in GitCheckout. This binding exists
// because the EnvPicker in the new UI needs to attach a branch string
// to a thread without forcing a checkout.
func (a *App) UpdateThreadBranch(id, branch string) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update branch: store unavailable")
	}
	if err := a.store.UpdateBranch(id, branch); err != nil {
		return store.Thread{}, err
	}
	return a.store.GetThread(id)
}

// UpdateThreadWorkspace persists a new workspace path. Used by the
// EnvPicker to switch a thread between the project root and a worktree
// without creating the worktree itself. Restarts the session if one is
// live because the provider CWD is part of its launch config.
func (a *App) UpdateThreadWorkspace(id, path string) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update workspace: store unavailable")
	}
	normalized := strings.TrimSpace(path)
	if normalized == "" {
		return store.Thread{}, fmt.Errorf("update workspace: path is required")
	}
	if err := a.store.UpdateWorkspacePath(id, normalized); err != nil {
		return store.Thread{}, err
	}
	return a.restartSessionIfAffected(id, "workspace")
}

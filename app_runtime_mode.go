package main

import (
	"fmt"
	"log"

	"agent-overflow/internal/provider"
)

// ThreadRuntimeModeChangedEvent is the payload emitted after the runtime
// mode for a thread is updated. Mirrors the interaction-mode event so the
// frontend has a uniform shape for "settings changed — do you need to
// reconnect?" banners. needsReconnect is true when an active session is
// running; the provider honors the new mode only after restart.
type ThreadRuntimeModeChangedEvent struct {
	ThreadID       string `json:"threadId"`
	RuntimeMode    string `json:"runtimeMode"`
	NeedsReconnect bool   `json:"needsReconnect"`
}

// GetThreadRuntimeMode returns the persisted runtime mode for a thread.
// Exposed as a convenience binding so the frontend picker doesn't have to
// round-trip the full Thread shape.
func (a *App) GetThreadRuntimeMode(threadID string) (string, error) {
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("get runtime mode: %w", err)
	}
	return string(provider.NormalizeRuntimeMode(t.RuntimeMode)), nil
}

// SetThreadRuntimeMode persists a new runtime mode and, if the thread has
// an active session, automatically restarts it so the provider picks up
// the new mode. An idempotent no-op when the mode is unchanged.
//
// Valid modes are the three provider.RuntimeMode values. Unknown strings
// are rejected outright rather than coerced — we want the binding to
// surface a clear error to the UI instead of silently swapping what the
// user picked for "full-access".
func (a *App) SetThreadRuntimeMode(threadID, mode string) (ThreadRuntimeModeChangedEvent, error) {
	normalized := provider.RuntimeMode(mode)
	switch normalized {
	case provider.RuntimeApprovalRequired, provider.RuntimeAutoAcceptEdits, provider.RuntimeFullAccess:
		// ok
	default:
		return ThreadRuntimeModeChangedEvent{}, fmt.Errorf("set runtime mode: invalid mode %q", mode)
	}

	t, err := a.store.GetThread(threadID)
	if err != nil {
		return ThreadRuntimeModeChangedEvent{}, fmt.Errorf("set runtime mode: %w", err)
	}

	if provider.NormalizeRuntimeMode(t.RuntimeMode) == normalized {
		// No-op: avoid tearing down a healthy session just because a UI
		// element re-submitted the current value.
		return ThreadRuntimeModeChangedEvent{
			ThreadID:       threadID,
			RuntimeMode:    string(normalized),
			NeedsReconnect: false,
		}, nil
	}

	if err := a.store.UpdateRuntimeMode(threadID, string(normalized)); err != nil {
		return ThreadRuntimeModeChangedEvent{}, fmt.Errorf("set runtime mode: %w", err)
	}

	needsReconnect := a.hasActiveSession(threadID)

	evt := ThreadRuntimeModeChangedEvent{
		ThreadID:       threadID,
		RuntimeMode:    string(normalized),
		NeedsReconnect: needsReconnect,
	}
	a.emitEvent("thread:runtime_mode_changed", evt)

	if needsReconnect {
		// Fire-and-forget the restart so the binding returns immediately.
		// The frontend can render the optimistic state and react to any
		// error via the existing session-status / error channels.
		go func() {
			if err := a.ReconnectSession(threadID); err != nil {
				log.Printf("runtime mode: reconnect %s: %v", threadID, err)
				a.emitErrorToThread(threadID, fmt.Sprintf("runtime mode change failed to reconnect: %v", err))
			}
		}()
	}

	return evt, nil
}

// defaultRuntimeModeForNewThread reads the settings file's default (or
// falls back to provider.DefaultRuntimeMode when settings are unavailable
// or hold an invalid value). Used by CreateThread so every new thread
// lands on a valid value without the settings panel being open.
func (a *App) defaultRuntimeModeForNewThread() string {
	if a.settings == nil {
		return string(provider.DefaultRuntimeMode)
	}
	s := a.settings.Get()
	return string(provider.NormalizeRuntimeMode(s.DefaultRuntimeMode))
}

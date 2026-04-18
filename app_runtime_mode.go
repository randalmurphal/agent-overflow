package main

import (
	"fmt"

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

// SetThreadRuntimeMode is the legacy binding name preserved only while
// the frontend migration lands (Wave 2c+). The implementation routes to
// UpdateThreadRuntimeMode, emits the same runtime-mode-changed event,
// and returns the older event struct.
//
// The new per-field binding UpdateThreadRuntimeMode in app_threads.go
// is the going-forward surface and returns store.Thread directly.
func (a *App) SetThreadRuntimeMode(threadID, mode string) (ThreadRuntimeModeChangedEvent, error) {
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return ThreadRuntimeModeChangedEvent{}, fmt.Errorf("set runtime mode: %w", err)
	}
	normalized := provider.RuntimeMode(mode)
	switch normalized {
	case provider.RuntimeApprovalRequired, provider.RuntimeAutoAcceptEdits, provider.RuntimeFullAccess:
		// ok
	default:
		return ThreadRuntimeModeChangedEvent{}, fmt.Errorf("set runtime mode: invalid mode %q", mode)
	}
	if provider.NormalizeRuntimeMode(t.RuntimeMode) == normalized {
		return ThreadRuntimeModeChangedEvent{
			ThreadID:       threadID,
			RuntimeMode:    string(normalized),
			NeedsReconnect: false,
		}, nil
	}

	if _, err := a.UpdateThreadRuntimeMode(threadID, string(normalized)); err != nil {
		return ThreadRuntimeModeChangedEvent{}, err
	}

	needsReconnect := a.hasActiveSession(threadID)
	evt := ThreadRuntimeModeChangedEvent{
		ThreadID:       threadID,
		RuntimeMode:    string(normalized),
		NeedsReconnect: needsReconnect,
	}
	a.emitEvent("thread:runtime_mode_changed", evt)
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

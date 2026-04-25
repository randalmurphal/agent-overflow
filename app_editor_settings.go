package main

import (
	"fmt"

	"agent-overflow/internal/editor"
	"agent-overflow/internal/settings"
)

// GetEditorSettings returns the user's persisted open-in-editor
// preferences. Empty preference means the catalog default applies at
// open time — surfaced verbatim so the settings UI can render an
// "Auto" pill rather than guessing on the frontend.
func (a *App) GetEditorSettings() settings.EditorSettings {
	if a.settings == nil {
		return settings.EditorSettings{}
	}
	return a.settings.Get().Editor
}

// SetEditorSettings persists the user's editor preference. The value
// is validated against the live catalog at open time, not here, so a
// preference that becomes invalid (editor uninstalled) silently falls
// back to the catalog default — see internal/editor.Resolve.
//
// On success, RefreshEditors invalidates the detection cache so the
// settings UI's next ListAvailableEditors call surfaces fresh state
// — the user just made a deliberate change and shouldn't see stale
// availability flags.
func (a *App) SetEditorSettings(s settings.EditorSettings) (settings.EditorSettings, error) {
	// Match the error shape used by every other settings-touching
	// binding ("settings service unavailable") rather than ErrShuttingDown,
	// which would mislead a frontend that branches on
	// shuttingDown-style messages — this path fires whenever the
	// settings service hasn't been wired (e.g. test rigs that construct
	// App without it), not only during shutdown.
	if a.settings == nil {
		return settings.EditorSettings{}, fmt.Errorf("settings service unavailable")
	}
	updated, err := a.settings.Update(map[string]any{
		"editor": map[string]any{
			"preference": s.Preference,
		},
	})
	if err != nil {
		return settings.EditorSettings{}, err
	}
	editor.RefreshEditors()
	return updated.Editor, nil
}

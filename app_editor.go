package main

import (
	"context"
	"fmt"

	"agent-overflow/internal/editor"
)

// EditorInfo is the wire shape of an editor row exposed to the
// frontend by ListAvailableEditors. We deliberately publish a subset
// of editor.Editor — ResolvedPath stays internal because it would
// leak filesystem layout (e.g. "/mnt/c/Users/<actual-username>/...")
// to any code that can read settings, including future remote
// surfaces. The frontend only needs ID / Name / Available to render
// the picker.
type EditorInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Available   bool   `json:"available"`
	EnvFallback bool   `json:"envFallback,omitempty"`
}

// ListAvailableEditors returns every editor the open-in-editor
// pipeline knows about. Available rows are launchable; unavailable
// rows are returned too so the settings picker can surface
// "VS Code (not installed)" instead of silently hiding options.
//
// On WSL backends this enforces the editor-bridge rule: the only
// editors that come back available are those reachable via the
// vendor's WSL bridge (PATH-resolved shim that targets /mnt/c, or a
// direct hit under /mnt/c/Users/.../AppData or /mnt/c/Program Files).
// Linux-native installs are intentionally absent from the available
// set even when their binary is on PATH — see
// /Users/randy/.claude/projects/-Users-randy-repos-agent-overflow/memory/feedback_wsl_editor_bridge.md.
func (a *App) ListAvailableEditors() ([]EditorInfo, error) {
	detected := editor.DetectEditors(context.Background())
	out := make([]EditorInfo, 0, len(detected))
	for _, e := range detected {
		out = append(out, EditorInfo{
			ID:          e.ID,
			Name:        e.Name,
			Available:   e.Available,
			EnvFallback: e.EnvFallback,
		})
	}
	return out, nil
}

// OpenInEditor launches the user's preferred editor against `path`,
// optionally placing the cursor at (line, col). Both are 1-indexed;
// pass 0 for either to open without cursor placement.
//
// Resolution order: settings.Editor.Preference → catalog priority →
// $EDITOR / $VISUAL fallback. On WSL the editor must be the
// Windows-installed app reachable via the vendor's WSL bridge; a
// Linux-native `code-oss` (or equivalent) on PATH is deliberately
// rejected because it would render via WSLg and miss the user's
// actual editor environment. See
// /Users/randy/.claude/projects/-Users-randy-repos-agent-overflow/memory/feedback_wsl_editor_bridge.md.
//
// Errors flow back to the frontend as user-facing toasts; the strings
// here are intentionally friendly — "no editor available" names
// install paths the user can act on rather than dumping the internal
// sentinel error.
func (a *App) OpenInEditor(path string, line, col int) error {
	if path == "" {
		return fmt.Errorf("open-in-editor: path is required")
	}

	preference := ""
	if a.settings != nil {
		preference = a.settings.Get().Editor.Preference
	}

	ctx := context.Background()
	detected := editor.DetectEditors(ctx)
	chosen, err := editor.Resolve(detected, preference)
	if err != nil {
		return err
	}
	return editor.Open(ctx, editor.SpawnOptions{
		Editor: chosen,
		Path:   path,
		Line:   line,
		Column: col,
	})
}

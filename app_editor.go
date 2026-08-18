package main

import (
	"context"

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
// set even when their binary is on PATH — see editor.DetectEditors
// for the WSL-bridge resolution rules.
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

// OpenInEditor launches an editor against `path`, optionally placing
// the cursor at (line, col). Both are 1-indexed; pass 0 for either to
// open without cursor placement.
//
// `workspacePath`, when non-empty, is the absolute base directory used
// to resolve a relative `path`. Click sites in the SPA that hand us
// repo-relative paths (diff cards, tool-result file paths, markdown
// path links inside chat) pass the active thread's workspace so the
// path round-trips correctly. Empty workspacePath + relative path is
// an error, and empty workspacePath + absolute path is the deliberate
// project-open trust path (no stat, folder opens allowed) — reserved
// for app affordances; the markdown pipeline never emits a
// workspace-less link. See editor.ResolvePath for the full contract:
// `~/` expansion pinned under home, UNC refusal, and the openability
// rule (existing regular files open from anywhere, folder opens are
// refused everywhere, new files only inside the workspace). This
// method is classified LocalOnly in internal/transport, so remote
// token-holders cannot call it.
//
// `editorID` selects which editor to launch:
//   - Empty → resolve via settings.Editor.Preference → catalog priority
//     → $EDITOR / $VISUAL fallback. This is the default path used by
//     every path-link and the header "Open" primary click.
//   - Non-empty → open in exactly that editor (the header dropdown's
//     "open this one, just this once" pick). It must be available or
//     the call errors; we never silently substitute a different editor
//     for an explicit choice, and this path deliberately ignores the
//     saved preference so a one-shot open doesn't change the default.
//
// On WSL the editor must be the Windows-installed app reachable via the
// vendor's WSL bridge; a Linux-native `code-oss` (or equivalent) on
// PATH is deliberately rejected because it would render via WSLg and
// miss the user's actual editor environment.
//
// Errors flow back to the frontend as user-facing toasts; the strings
// here are intentionally friendly — "no editor available" names
// install paths the user can act on rather than dumping the internal
// sentinel error.
func (a *App) OpenInEditor(path string, line, col int, workspacePath, editorID string) error {
	// editor.ResolvePath (called inside editor.Open) is the single source
	// of truth for the path-shape contract: empty / non-canonical / UNC
	// inputs and openability refusals (missing outside the workspace,
	// directories anywhere) all surface as errors there. We don't
	// pre-check here so the boundary stays in one place.

	ctx := context.Background()
	detected := editor.DetectEditors(ctx)

	var chosen *editor.Editor
	var err error
	if editorID != "" {
		chosen, err = editor.ResolveExact(detected, editorID)
	} else {
		preference := ""
		if a.settings != nil {
			preference = a.settings.Get().Editor.Preference
		}
		chosen, err = editor.Resolve(detected, preference)
	}
	if err != nil {
		return err
	}
	return editor.Open(ctx, editor.SpawnOptions{
		Editor:        chosen,
		Path:          path,
		Line:          line,
		Column:        col,
		WorkspacePath: workspacePath,
	})
}

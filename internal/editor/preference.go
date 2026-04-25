package editor

import (
	"errors"
	"fmt"
)

// ErrNoEditor is returned by Resolve / Open when no editor is
// available. It carries a deliberately neutral message so the UI
// surface (Phase H) can pair it with platform-specific install
// guidance — the WSL "install VS Code on Windows + Remote-WSL"
// hint, the macOS "install VS Code or Cursor" hint, etc. without
// a backend translation table.
var ErrNoEditor = errors.New("no editor available")

// ErrCommandNotFound wraps the TOCTOU re-check failure inside Open.
// Resolve never returns it directly — that path goes through
// ErrNoEditor — but the spawn step uses it to distinguish
// "uninstalled between detect and spawn" from a generic exec error
// the user wouldn't recognise.
var ErrCommandNotFound = errors.New("editor command not found")

// Resolve picks the editor that should be invoked for an open-in-
// editor call. The decision tree:
//
//  1. preferredID matches an available named editor → use it.
//  2. preferredID is empty or the named editor is not available → walk
//     the catalog priority order and pick the first available one.
//  3. nothing in the catalog is available, but $EDITOR / $VISUAL
//     resolved to a binary on PATH → use that synthetic entry as a
//     final fallback. The catalog priority outranks $EDITOR because
//     the named editors carry richer launch styles (--goto / line:col)
//     and a vi-in-a-terminal fallback would be a worse default for a
//     GUI app.
//  4. Otherwise return ErrNoEditor.
//
// detected is the slice DetectEditors returned. Callers pass it
// in (rather than re-invoking detection) so the resolve path doesn't
// re-stat /mnt/c on every Open.
func Resolve(detected []Editor, preferredID string) (*Editor, error) {
	availableByID := make(map[string]int, len(detected))
	for i, e := range detected {
		if !e.Available {
			continue
		}
		availableByID[e.ID] = i
	}

	if preferredID != "" {
		if idx, ok := availableByID[preferredID]; ok {
			ed := detected[idx]
			return &ed, nil
		}
	}

	for _, def := range editorCatalog {
		if idx, ok := availableByID[def.ID]; ok {
			ed := detected[idx]
			return &ed, nil
		}
	}

	for i := range detected {
		if !detected[i].Available {
			continue
		}
		if detected[i].EnvFallback {
			ed := detected[i]
			return &ed, nil
		}
	}

	return nil, fmt.Errorf("%w; install VS Code, Cursor, Sublime Text, or set $EDITOR", ErrNoEditor)
}

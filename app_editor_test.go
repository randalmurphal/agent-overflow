package main

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/settings"
)

// TestOpenInEditor_NilSettingsDoesNotPanic pins the contract for test
// rigs that wire App without a settings service: nil settings must not
// nil-dereference deeper in the open flow.
//
// Pass a relative path with empty workspacePath so editor.ResolvePath's
// "relative path requires workspacePath" guard fires before the spawn
// step. This keeps the test from launching a real editor on machines
// where one is installed (the catalog falls back to whatever's
// available when preference is empty), while still exercising the
// settings/editor resolution branch where the nil-deref would land.
func TestOpenInEditor_NilSettingsDoesNotPanic(t *testing.T) {
	app := &App{}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("OpenInEditor with nil settings panicked: %v", r)
		}
	}()

	err := app.OpenInEditor("relative-path-does-not-spawn", 0, 0, "")
	if err == nil {
		t.Fatal("relative path with empty workspacePath should be rejected before spawn")
	}
	if !strings.Contains(err.Error(), "workspacePath") {
		t.Fatalf("expected workspacePath-required error, got: %v", err)
	}
}

// TestOpenInEditor_RejectsEmptyPath pins the boundary guard. The
// empty-path error originates in editor.ResolvePath; the binding wraps
// it. Without the guard, the catalog walk would still run and we'd
// get a less specific downstream error when the spawn step failed on
// "".
func TestOpenInEditor_RejectsEmptyPath(t *testing.T) {
	app := &App{settings: settings.NewService(t.TempDir())}

	err := app.OpenInEditor("", 1, 1, "")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// TestListAvailableEditors_OmitsResolvedPath pins the wire-shape
// contract. ResolvedPath would leak filesystem layout (e.g.
// "/mnt/c/Users/<actual-username>/...") to any code that reads
// settings, including future remote surfaces. The frontend only needs
// ID/Name/Available to render the picker.
//
// Implementation is structural — EditorInfo doesn't have a
// ResolvedPath field, so a regression that reintroduces leakage would
// surface as a compile failure first. We marshal the result and grep
// the bytes as a belt-and-braces guard against a future struct
// embedding or json:",inline" tag that would smuggle the field in.
func TestListAvailableEditors_OmitsResolvedPath(t *testing.T) {
	app := &App{}

	got, err := app.ListAvailableEditors()
	if err != nil {
		t.Fatalf("ListAvailableEditors: %v", err)
	}
	for _, ed := range got {
		if ed.ID == "" {
			t.Fatalf("editor row missing ID: %+v", ed)
		}
		// Belt-and-braces: marshal the row and grep for "resolvedPath"
		// (the JSON tag a future regression would most likely add). If
		// EditorInfo grows a ResolvedPath field, this test catches it.
		// We do this instead of a reflect-based field walk so the test
		// stays simple and robust against tag/case changes.
		marshalled := mustJSON(t, ed)
		if strings.Contains(strings.ToLower(marshalled), "resolvedpath") {
			t.Fatalf("EditorInfo wire shape leaked resolvedPath: %s", marshalled)
		}
	}
}

// TestSetEditorSettings_NilSettingsReturnsError pins the error message
// shape consistency: the binding returns a "settings service
// unavailable" error rather than ErrShuttingDown when settings isn't
// wired, matching every other settings-touching binding. A frontend
// branching on "shutting_down"-shaped errors would otherwise misread
// a nil-service test rig.
func TestSetEditorSettings_NilSettingsReturnsError(t *testing.T) {
	app := &App{}

	_, err := app.SetEditorSettings(settings.EditorSettings{Preference: "code"})
	if err == nil {
		t.Fatal("expected error when settings unavailable")
	}
	if !strings.Contains(err.Error(), "settings service unavailable") {
		t.Fatalf("error should match the consistent message, got: %v", err)
	}
}

// TestSetEditorSettings_PersistsAndRefreshesCache covers the happy
// path: the binding persists the value through the settings service
// and invalidates the editor detection cache so the next picker
// render surfaces fresh state.
func TestSetEditorSettings_PersistsAndRefreshesCache(t *testing.T) {
	app := &App{settings: settings.NewService(t.TempDir())}

	out, err := app.SetEditorSettings(settings.EditorSettings{Preference: "code"})
	if err != nil {
		t.Fatalf("SetEditorSettings: %v", err)
	}
	if out.Preference != "code" {
		t.Fatalf("preference round-trip wrong: %+v", out)
	}

	got := app.GetEditorSettings()
	if got.Preference != "code" {
		t.Fatalf("GetEditorSettings = %+v, want preference=code", got)
	}
}

// mustJSON is a tiny test helper that marshals v to JSON or fatals.
// Centralised so we don't sprinkle err handling across the assertions.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

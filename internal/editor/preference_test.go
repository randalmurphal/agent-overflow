package editor

import (
	"errors"
	"testing"
)

func TestResolve_PreferredAvailable(t *testing.T) {
	detected := []Editor{
		{ID: "code", Name: "VS Code", Available: true, ResolvedPath: "/usr/bin/code"},
		{ID: "cursor", Name: "Cursor", Available: true, ResolvedPath: "/usr/bin/cursor"},
	}
	got, err := Resolve(detected, "cursor")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ID != "cursor" {
		t.Fatalf("expected cursor; got %s", got.ID)
	}
}

func TestResolve_PreferredUnavailableFallsBackToCatalogPriority(t *testing.T) {
	// Preference points at an editor that exists in the catalog but
	// isn't currently available; resolution must fall through to the
	// first available catalog editor (code → priority 0).
	detected := []Editor{
		{ID: "code", Available: true, ResolvedPath: "/usr/bin/code"},
		{ID: "cursor", Available: false},
	}
	got, err := Resolve(detected, "cursor")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ID != "code" {
		t.Fatalf("expected fallback to code; got %s", got.ID)
	}
}

func TestResolve_EmptyPreferenceUsesCatalogOrder(t *testing.T) {
	// Catalog priority is code → code-insiders → cursor → windsurf →
	// codium → subl → zed. With code-insiders first available, we
	// must skip over later-priority candidates that happen to come
	// earlier in the detected slice.
	detected := []Editor{
		{ID: "zed", Available: true, ResolvedPath: "/usr/bin/zed"},
		{ID: "code-insiders", Available: true, ResolvedPath: "/usr/bin/code-insiders"},
		{ID: "cursor", Available: true, ResolvedPath: "/usr/bin/cursor"},
	}
	got, err := Resolve(detected, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ID != "code-insiders" {
		t.Fatalf("expected code-insiders by catalog priority; got %s", got.ID)
	}
}

func TestResolve_FallsBackToEnvWhenNoCatalogEditor(t *testing.T) {
	envEntry := Editor{
		ID: "env:editor", Name: "$EDITOR", Command: "vim",
		Available: true, ResolvedPath: "/usr/bin/vim",
		EnvFallback: true, LaunchStyle: LaunchStyleDirectPath,
	}
	detected := []Editor{
		{ID: "code", Available: false},
		envEntry,
	}
	got, err := Resolve(detected, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.EnvFallback {
		t.Fatalf("expected env fallback chosen; got %+v", got)
	}
}

func TestResolve_PrefersCatalogOverEnvFallback(t *testing.T) {
	// Even when $EDITOR resolves, a named catalog editor wins —
	// catalog editors carry richer launch styles (--goto / line:col)
	// and a vi-in-a-terminal fallback would be a worse default.
	envEntry := Editor{
		ID: "env:editor", Name: "$EDITOR", Command: "vim",
		Available: true, ResolvedPath: "/usr/bin/vim", EnvFallback: true,
	}
	detected := []Editor{
		envEntry,
		{ID: "code", Available: true, ResolvedPath: "/usr/bin/code"},
	}
	got, err := Resolve(detected, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ID != "code" {
		t.Fatalf("expected catalog code over env vim; got %s", got.ID)
	}
}

func TestResolve_PreferenceWinsOverPriority(t *testing.T) {
	detected := []Editor{
		{ID: "code", Available: true, ResolvedPath: "/usr/bin/code"},
		{ID: "subl", Available: true, ResolvedPath: "/usr/bin/subl"},
	}
	got, err := Resolve(detected, "subl")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ID != "subl" {
		t.Fatalf("preference should win; got %s", got.ID)
	}
}

func TestResolve_NothingAvailableReturnsErrNoEditor(t *testing.T) {
	detected := []Editor{
		{ID: "code", Available: false},
		{ID: "cursor", Available: false},
	}
	_, err := Resolve(detected, "code")
	if !errors.Is(err, ErrNoEditor) {
		t.Fatalf("expected ErrNoEditor; got %v", err)
	}
}

func TestResolve_EmptyDetectedReturnsErrNoEditor(t *testing.T) {
	_, err := Resolve(nil, "")
	if !errors.Is(err, ErrNoEditor) {
		t.Fatalf("expected ErrNoEditor; got %v", err)
	}
}

func TestResolveExact_ReturnsNamedEditorWhenAvailable(t *testing.T) {
	detected := []Editor{
		{ID: "code", Available: true, ResolvedPath: "/usr/bin/code"},
		{ID: "cursor", Available: true, ResolvedPath: "/usr/bin/cursor"},
	}
	got, err := ResolveExact(detected, "cursor")
	if err != nil {
		t.Fatalf("ResolveExact: %v", err)
	}
	if got.ID != "cursor" {
		t.Fatalf("expected cursor; got %s", got.ID)
	}
}

func TestResolveExact_UnavailableNamedEditorErrors(t *testing.T) {
	// The one-shot header pick must NOT fall through to the catalog the
	// way Resolve does — an explicit choice that isn't available is an
	// error, never a silent substitution to `code`.
	detected := []Editor{
		{ID: "code", Available: true, ResolvedPath: "/usr/bin/code"},
		{ID: "cursor", Available: false},
	}
	_, err := ResolveExact(detected, "cursor")
	if !errors.Is(err, ErrNoEditor) {
		t.Fatalf("expected ErrNoEditor for unavailable pick; got %v", err)
	}
}

func TestResolveExact_UnknownEditorErrors(t *testing.T) {
	detected := []Editor{{ID: "code", Available: true, ResolvedPath: "/usr/bin/code"}}
	_, err := ResolveExact(detected, "not-a-real-editor")
	if !errors.Is(err, ErrNoEditor) {
		t.Fatalf("expected ErrNoEditor for unknown id; got %v", err)
	}
}

func TestResolveExact_EmptyIDErrors(t *testing.T) {
	// Empty id is a caller bug — Resolve is the entry point for "no
	// specific editor in mind". ResolveExact refuses rather than
	// matching the first zero-ID slice entry.
	detected := []Editor{{ID: "code", Available: true}}
	_, err := ResolveExact(detected, "")
	if !errors.Is(err, ErrNoEditor) {
		t.Fatalf("expected ErrNoEditor for empty id; got %v", err)
	}
}

func TestResolveExact_ReturnsCopyNotPointerIntoDetected(t *testing.T) {
	detected := []Editor{
		{ID: "code", Name: "VS Code", Available: true, ResolvedPath: "/usr/bin/code"},
	}
	got, err := ResolveExact(detected, "code")
	if err != nil {
		t.Fatalf("ResolveExact: %v", err)
	}
	got.Name = "Mutated"
	if detected[0].Name == "Mutated" {
		t.Fatalf("ResolveExact handed back pointer into caller's slice")
	}
}

func TestResolve_ReturnsCopyNotPointerIntoDetected(t *testing.T) {
	// Mutating the returned editor must not race writes into the
	// caller's detected slice — the App method passes the slice
	// directly from DetectEditors and a shared pointer would be a
	// foot-gun.
	detected := []Editor{
		{ID: "code", Name: "VS Code", Available: true, ResolvedPath: "/usr/bin/code"},
	}
	got, err := Resolve(detected, "code")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got.Name = "Mutated"
	if detected[0].Name == "Mutated" {
		t.Fatalf("Resolve handed back pointer into caller's slice")
	}
}

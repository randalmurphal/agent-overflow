package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

func TestAppListProjects(t *testing.T) {
	app := newTestAppWithStore(t)
	dir := t.TempDir()
	p, err := app.CreateProject(dir)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	list, err := app.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	found := false
	for _, pwc := range list {
		if pwc.Project.ID == p.ID {
			found = true
			if pwc.ThreadCount != 0 {
				t.Errorf("ThreadCount = %d, want 0", pwc.ThreadCount)
			}
		}
	}
	if !found {
		t.Fatalf("ListProjects() did not include created project %s", p.ID)
	}
}

func TestAppCreateProjectRejectsNonexistentPath(t *testing.T) {
	app := newTestAppWithStore(t)
	_, err := app.CreateProject("/nonexistent/path/should/not/exist-xyz")
	if err == nil {
		t.Fatal("CreateProject() error = nil, want stat error")
	}
}

func TestAppCreateProjectRejectsFile(t *testing.T) {
	app := newTestAppWithStore(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := app.CreateProject(file)
	if err == nil {
		t.Fatal("CreateProject(file) error = nil, want is-not-a-directory error")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error = %v, want 'not a directory'", err)
	}
}

func TestAppCreateProjectSurfacesDuplicate(t *testing.T) {
	app := newTestAppWithStore(t)
	dir := t.TempDir()
	if _, err := app.CreateProject(dir); err != nil {
		t.Fatalf("first CreateProject: %v", err)
	}
	_, err := app.CreateProject(dir)
	if !errors.Is(err, store.ErrProjectPathInUse) {
		t.Fatalf("CreateProject(dup) = %v, want ErrProjectPathInUse", err)
	}
}

func TestAppRenameProject(t *testing.T) {
	app := newTestAppWithStore(t)
	dir := t.TempDir()
	p, err := app.CreateProject(dir)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	renamed, err := app.RenameProject(p.ID, "Shiny")
	if err != nil {
		t.Fatalf("RenameProject: %v", err)
	}
	if renamed.Name != "Shiny" {
		t.Fatalf("Name = %q, want Shiny", renamed.Name)
	}
}

func TestAppArchiveUnarchiveProject(t *testing.T) {
	app := newTestAppWithStore(t)
	dir := t.TempDir()
	p, err := app.CreateProject(dir)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := app.ArchiveProject(p.ID); err != nil {
		t.Fatalf("ArchiveProject: %v", err)
	}
	list, _ := app.ListProjects()
	for _, pwc := range list {
		if pwc.Project.ID == p.ID {
			t.Fatal("archived project still in ListProjects()")
		}
	}
	unarchived, err := app.UnarchiveProject(p.ID)
	if err != nil {
		t.Fatalf("UnarchiveProject: %v", err)
	}
	if unarchived.Archived {
		t.Fatal("UnarchiveProject returned Archived=true")
	}
}

func TestAppDeleteProjectReturnsThreadIDs(t *testing.T) {
	app := newTestAppWithStore(t)
	dir := t.TempDir()
	p, err := app.CreateProject(dir)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	thread, err := app.CreateThread(CreateThreadOptions{
		ProjectID: p.ID,
		Provider:  "claude",
		Model:     "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	ids, err := app.DeleteProject(p.ID)
	if err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if len(ids) != 1 || ids[0] != thread.ID {
		t.Fatalf("DeleteProject ids = %v, want [%s]", ids, thread.ID)
	}

	// Thread row must be gone (cascade).
	if _, err := app.store.GetThread(thread.ID); err == nil {
		t.Fatal("thread survived DeleteProject")
	}
}

func TestAppCreateThreadRequiresProjectID(t *testing.T) {
	app := newTestAppWithStore(t)
	_, err := app.CreateThread(CreateThreadOptions{})
	if err == nil {
		t.Fatal("CreateThread() error = nil, want projectId required")
	}
	if !strings.Contains(err.Error(), "projectId is required") {
		t.Fatalf("error = %v, want 'projectId is required'", err)
	}
}

func TestAppCreateThreadResolvesProvider(t *testing.T) {
	app := newTestAppWithStore(t)
	dir := t.TempDir()
	p, err := app.CreateProject(dir)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	thread, err := app.CreateThread(CreateThreadOptions{
		ProjectID: p.ID,
		// Provider intentionally empty; should fall back to settings default.
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	// Default provider from settings is "claude" (see settings.DefaultSettings).
	if thread.Provider != "claude" {
		t.Fatalf("Provider = %q, want claude (default)", thread.Provider)
	}
	if thread.WorkspacePath != p.Path {
		t.Fatalf("WorkspacePath = %q, want project path %q", thread.WorkspacePath, p.Path)
	}
}

package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
)

func TestWorkflowJobNotesBoundedAndUnknownRejected(t *testing.T) {
	app, _ := setupE2EApp(t)
	automation := store.Automation{
		ID: "automation", ProjectID: "project", WorkflowID: "wf", WorkflowScope: "shared",
		Name: "Nightly", Enabled: true, Trigger: json.RawMessage(`{}`), CreatedAt: 1, UpdatedAt: 1,
	}
	if err := app.store.CreateAutomation(automation); err != nil {
		t.Fatal(err)
	}
	if err := app.WorkflowSetJobNotes(automation.ID, "continue here"); err != nil {
		t.Fatal(err)
	}
	if notes, err := app.WorkflowGetJobNotes(automation.ID); err != nil || notes != "continue here" {
		t.Fatalf("notes = %q err=%v", notes, err)
	}
	if err := app.WorkflowSetJobNotes(automation.ID, strings.Repeat("x", notify.MaxBodyBytes+1)); err == nil {
		t.Fatal("oversized notes unexpectedly succeeded")
	}
	if _, err := app.WorkflowGetJobNotes("missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown notes error = %v, want sql.ErrNoRows", err)
	}
	if err := app.WorkflowSetJobNotes("missing", "notes"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown set error = %v, want sql.ErrNoRows", err)
	}
}

func TestWorkflowListDefinitionsIncludesValidationAndPredictedPosition(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	app.configDir = configRoot
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow, err := app.store.GetProject(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkflowListingFixtures(t, configRoot, projectRow.Slug)
	if err := app.store.CreateWorkItem(store.WorkItem{
		ID: "queued", ProjectID: projectRow.ID, Goal: "queued", WorkflowID: "valid", WorkflowScope: "shared",
		State: string(engine.StateQueued), Source: "manual", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	otherProject := testutil.EnsureProject(t, app.store, testutil.InitGitRepo(t))
	if err := app.store.CreateWorkItem(store.WorkItem{
		ID: "queued-later", ProjectID: otherProject.ID, Goal: "later", WorkflowID: "valid", WorkflowScope: "shared",
		State: string(engine.StateQueued), SortPosition: 99, Source: "manual", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	listings, err := app.WorkflowListDefinitions(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listings) != 2 {
		t.Fatalf("listings = %+v", listings)
	}
	byID := make(map[string]WorkflowDefinitionListing, len(listings))
	for _, listing := range listings {
		byID[listing.ID] = listing
	}
	valid := byID["valid"]
	if !valid.Valid || !valid.AllBindingsAvailable || valid.PhaseCount != 1 || len(valid.Phases) != 1 || valid.Phases[0].Provider != "codex" || valid.PredictedQueuePosition != 2 {
		t.Fatalf("valid listing = %+v", valid)
	}
	invalid := byID["invalid"]
	if invalid.Valid || invalid.AllBindingsAvailable || !strings.Contains(invalid.FirstValidationError, "not bindable") {
		t.Fatalf("invalid listing = %+v", invalid)
	}
}

func TestWorkflowRemoveQueuedItemKeepsCancelledRecord(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeWorkspaceWorkflow(t, configRoot, "done")
	if _, err := app.settings.Update(map[string]any{"workflowQueueActive": false}); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
	projectRow := testutil.EnsureProject(t, app.store, testutil.InitGitRepo(t))
	if _, err := app.WorkflowEnqueueItem(projectRow.ID, "missing", "shared", "invalid", json.RawMessage(`{}`), nil, false); err == nil {
		t.Fatal("unknown workflow id did not return synchronously")
	}
	if count, err := app.store.CountWorkItemsInStates(string(engine.StateQueued)); err != nil || count != 0 {
		t.Fatalf("unknown workflow persistence count = %d err=%v, want zero", count, err)
	}
	item, err := app.WorkflowEnqueueItem(projectRow.ID, "workspace-flow", "shared", "remove me", json.RawMessage(`{}`), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.WorkflowRemoveQueuedItem(item.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(engine.StateCancelled) || stored.WorktreePath != "" {
		t.Fatalf("removed item = %+v", stored)
	}
	if err := app.WorkflowRemoveQueuedItem(item.ID); err == nil {
		t.Fatal("second removal unexpectedly succeeded")
	}
}

func writeWorkflowListingFixtures(t *testing.T, configRoot, slug string) {
	t.Helper()
	shared := filepath.Join(configRoot, "workflows")
	projectDir := filepath.Join(configRoot, "projects", slug)
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := `id: valid
name: Valid workflow
default_step_mode: true
phases:
  - id: work
    driver: agent
    provider: codex
    model: gpt-5
    prompt: prompt.md
    access: read-only
    gate:
      routes:
        - to: done
`
	invalid := `id: invalid
name: Invalid workflow
phases:
  - id: check
    driver: tool
    check: missing-check
    access: read-only
    gate:
      routes:
        - to: done
`
	for name, content := range map[string]string{"valid.yaml": valid, "invalid.yaml": invalid, "prompt.md": "Do the work."} {
		if err := os.WriteFile(filepath.Join(shared, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, "profile.yaml"), []byte("base_branch: main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

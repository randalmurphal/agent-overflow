package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

func TestAutomationCRUDNotesAndCursors(t *testing.T) {
	s := newTestStore(t)
	automation := Automation{
		ID: "auto", ProjectID: "project-a", WorkflowID: "triage",
		WorkflowScope: "shared", Name: "Nightly triage", Enabled: true,
		Trigger:   json.RawMessage(`{"cron":"0 2 * * *"}`),
		Condition: json.RawMessage(`{"branch":"main"}`),
		Seeds:     json.RawMessage(`{"limit":10}`), Notes: "start here",
		CreatedAt: 10, UpdatedAt: 10,
	}
	if err := s.CreateAutomation(automation); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateAutomation(Automation{
		ID: "other", ProjectID: "project-b", WorkflowID: "triage",
		WorkflowScope: "project", Name: "Other", Enabled: true,
		Trigger: json.RawMessage(`{"event":"changed"}`), CreatedAt: 20, UpdatedAt: 20,
	}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	got, err := s.GetAutomation("auto")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != automation.Name || !got.Enabled || string(got.Trigger) != string(automation.Trigger) || got.Notes != "start here" {
		t.Fatalf("automation = %#v", got)
	}
	list, err := s.ListAutomations("project-a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != "auto" {
		t.Fatalf("project list = %#v", list)
	}

	automation.Name = "Updated"
	automation.Enabled = false
	automation.Trigger = json.RawMessage(`{"event":"ticket"}`)
	automation.UpdatedAt = 30
	if err := s.UpdateAutomation(automation); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := s.SetAutomationEnabled("auto", true, 40); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := s.SetAutomationNotes("auto", "continue from AO-7", 50); err != nil {
		t.Fatalf("set notes: %v", err)
	}
	notes, err := s.GetAutomationNotes("auto")
	if err != nil {
		t.Fatalf("get notes: %v", err)
	}
	if notes != "continue from AO-7" {
		t.Fatalf("notes = %q", notes)
	}
	got, err = s.GetAutomation("auto")
	if err != nil {
		t.Fatalf("get updated: %v", err)
	}
	if got.Name != "Updated" || !got.Enabled || got.UpdatedAt != 50 {
		t.Fatalf("updated automation = %#v", got)
	}

	cursor := AutomationCursor{AutomationID: "auto", SourceKey: "jira", Cursor: "one", UpdatedAt: 60}
	if err := s.SetAutomationCursor(cursor); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	cursor.Cursor = "two"
	cursor.UpdatedAt = 70
	if err := s.SetAutomationCursor(cursor); err != nil {
		t.Fatalf("update cursor: %v", err)
	}
	gotCursor, err := s.GetAutomationCursor("auto", "jira")
	if err != nil {
		t.Fatalf("get cursor: %v", err)
	}
	if gotCursor.Cursor != "two" || gotCursor.UpdatedAt != 70 {
		t.Fatalf("cursor = %#v", gotCursor)
	}

	if err := s.DeleteAutomation("auto"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetAutomation("auto"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("get deleted error = %v, want sql.ErrNoRows", err)
	}
	if _, err := s.GetAutomationCursor("auto", "jira"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cursor survived automation delete: %v", err)
	}
	missing := Automation{
		ID: "missing", ProjectID: "project-a", WorkflowID: "triage",
		WorkflowScope: "shared", Name: "Missing", Trigger: json.RawMessage(`{}`),
		UpdatedAt: 80,
	}
	for name, err := range map[string]error{
		"update":    s.UpdateAutomation(missing),
		"delete":    s.DeleteAutomation("missing"),
		"enable":    s.SetAutomationEnabled("missing", true, 80),
		"set notes": s.SetAutomationNotes("missing", "notes", 80),
	} {
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("%s missing automation error = %v, want sql.ErrNoRows", name, err)
		}
	}
}

func TestAutomationFireRecordAndActiveRun(t *testing.T) {
	s := newTestStore(t)
	enabled := Automation{
		ID: "auto-fire", ProjectID: "project-a", WorkflowID: "nightly",
		WorkflowScope: "project", Name: "Nightly audit", Enabled: true,
		Trigger: json.RawMessage(`{"kind":"cron","expr":"0 3 * * *"}`), CreatedAt: 10, UpdatedAt: 10,
	}
	disabled := Automation{
		ID: "auto-off", ProjectID: "project-a", WorkflowID: "nightly",
		WorkflowScope: "project", Name: "Paused audit", Enabled: false,
		Trigger: json.RawMessage(`{"kind":"cron","expr":"0 4 * * *"}`), CreatedAt: 20, UpdatedAt: 20,
	}
	for _, automation := range []Automation{enabled, disabled} {
		if err := s.CreateAutomation(automation); err != nil {
			t.Fatalf("create %s: %v", automation.ID, err)
		}
	}

	// A fresh automation carries a zeroed fire record, and only the enabled one
	// is in the set the scheduler schedules.
	fresh, err := s.GetAutomation("auto-fire")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fresh.LastFiredAt != 0 || fresh.LastRunItemID != "" || fresh.SkipCount != 0 ||
		fresh.LastSkipAt != 0 || fresh.LastSkipReason != "" {
		t.Fatalf("fresh automation fire record = %#v", fresh)
	}
	live, err := s.ListEnabledAutomations()
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(live) != 1 || live[0].ID != "auto-fire" {
		t.Fatalf("enabled automations = %#v", live)
	}

	if err := s.RecordAutomationFire("auto-fire", 1_700, "run-1"); err != nil {
		t.Fatalf("record fire: %v", err)
	}
	if err := s.RecordAutomationSkip("auto-fire", 1_800, "run run-1 is still running"); err != nil {
		t.Fatalf("record skip: %v", err)
	}
	if err := s.RecordAutomationSkip("auto-fire", 1_900, "condition false"); err != nil {
		t.Fatalf("record second skip: %v", err)
	}
	recorded, err := s.GetAutomation("auto-fire")
	if err != nil {
		t.Fatalf("get after record: %v", err)
	}
	if recorded.LastFiredAt != 1_700 || recorded.LastRunItemID != "run-1" {
		t.Fatalf("fire record = %#v", recorded)
	}
	if recorded.SkipCount != 2 || recorded.LastSkipAt != 1_900 || recorded.LastSkipReason != "condition false" {
		t.Fatalf("skip record = %#v", recorded)
	}
	// A fire is not an edit: the definition's updated_at is untouched, so the
	// scheduler cannot mistake activity for a changed schedule.
	if recorded.UpdatedAt != 10 {
		t.Fatalf("updatedAt = %d, want 10", recorded.UpdatedAt)
	}

	// A definition update leaves the fire record alone.
	updated := recorded
	updated.Name = "Nightly audit v2"
	updated.UpdatedAt = 40
	if err := s.UpdateAutomation(updated); err != nil {
		t.Fatalf("update: %v", err)
	}
	afterUpdate, err := s.GetAutomation("auto-fire")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if afterUpdate.SkipCount != 2 || afterUpdate.LastRunItemID != "run-1" || afterUpdate.Name != "Nightly audit v2" {
		t.Fatalf("automation after update = %#v", afterUpdate)
	}

	for name, err := range map[string]error{
		"fire": s.RecordAutomationFire("missing", 1, "run-x"),
		"skip": s.RecordAutomationSkip("missing", 1, "why"),
	} {
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("record %s on a missing automation = %v, want sql.ErrNoRows", name, err)
		}
	}

	// The overlap policy's question: any non-terminal run this automation
	// started. Terminal runs and other automations' runs are not answers.
	if _, found, err := s.ActiveAutomationRun("auto-fire"); err != nil || found {
		t.Fatalf("active run before any run = (%v, %v)", found, err)
	}
	for _, run := range []struct {
		id, state, reason, sourceRef string
		createdAt                    int64
	}{
		{"run-done", "done", "", "auto-fire", 100},
		{"run-other", "running", "", "auto-off", 110},
		{"run-parked", "needs-human", "question", "auto-fire", 120},
	} {
		if err := s.CreateWorkItem(WorkItem{
			ID: run.id, ProjectID: "project-a", Goal: "Nightly audit",
			WorkflowID: "nightly", WorkflowScope: "project", State: run.state,
			Reason: run.reason, Source: "automation", SourceRef: run.sourceRef,
			CreatedAt: run.createdAt,
		}); err != nil {
			t.Fatalf("create work item %s: %v", run.id, err)
		}
	}
	active, found, err := s.ActiveAutomationRun("auto-fire")
	if err != nil || !found {
		t.Fatalf("active run = (%#v, %v, %v)", active, found, err)
	}
	// A park is still an unfinished run: overlapping through one is overlap.
	if active.ItemID != "run-parked" || active.State != "needs-human" || active.Reason != "question" {
		t.Fatalf("active run = %#v", active)
	}
	if err := s.UpdateWorkItemState("run-parked", "done", "", 130); err != nil {
		t.Fatalf("settle parked run: %v", err)
	}
	if _, found, err := s.ActiveAutomationRun("auto-fire"); err != nil || found {
		t.Fatalf("active run after settle = (%v, %v)", found, err)
	}
}

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

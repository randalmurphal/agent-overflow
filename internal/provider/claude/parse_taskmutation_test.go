package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// TestParseAssistantTaskCreateNoEventUntilResult covers the staged
// behaviour: the assistant tool_use for TaskCreate must not emit
// either EventToolStart or any per-task event. The per-task event only
// fires after the matching tool_result confirms the create with an
// authoritative id.
func TestParseAssistantTaskCreateNoEventUntilResult(t *testing.T) {
	p := NewParser()

	useLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tool-create-1","name":"TaskCreate","input":{"subject":"Fix lifespan leak"}}` +
		`]}}`)
	events, err := p.ParseLine(testThread, useLine)
	if err != nil {
		t.Fatalf("parse use: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventToolStart || e.Kind == provider.EventTaskCreate || e.Kind == provider.EventTodoUpdate {
			t.Fatalf("expected no tool/task/todo event on TaskCreate use; got %+v", e)
		}
	}
}

// TestParseTaskCreateEmitsPerTaskEvent covers the happy-path
// roundtrip: assistant tool_use stages, tool_result emits a typed
// EventTaskCreate carrying the authoritative id and subject. No
// EventTodoUpdate from the parser — triage projects the snapshot.
func TestParseTaskCreateEmitsPerTaskEvent(t *testing.T) {
	p := NewParser()

	useLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tool-create-1","name":"TaskCreate","input":{"subject":"Fix lifespan leak"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("parse use: %v", err)
	}

	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tool-create-1","content":"Task #1 created successfully: Fix lifespan leak"}` +
		`]},"tool_use_result":{"task":{"id":"1","subject":"Fix lifespan leak"}}}`)
	events, err := p.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}

	evt := pickEvent(t, events, provider.EventTaskCreate)
	if evt.ItemID != "tool-create-1" {
		t.Errorf("ItemID: got %q, want tool-create-1", evt.ItemID)
	}
	var meta provider.TaskCreateMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.TaskID != "1" || meta.Subject != "Fix lifespan leak" {
		t.Errorf("meta: got %+v, want {TaskID:1 Subject:Fix lifespan leak}", meta)
	}
	for _, e := range events {
		if e.Kind == provider.EventToolComplete && e.ItemID == "tool-create-1" {
			t.Errorf("TaskCreate must not emit EventToolComplete; got %+v", e)
		}
		if e.Kind == provider.EventTodoUpdate {
			t.Errorf("parser must not emit EventTodoUpdate; triage owns the snapshot; got %+v", e)
		}
	}
}

// TestParseTaskUpdateEmitsStatus covers the status-transition emit:
// a TaskUpdate with status:"in_progress" produces an EventTaskUpdate
// with the normalised camelCase status.
func TestParseTaskUpdateEmitsStatus(t *testing.T) {
	p := NewParser()
	seedTaskCreateRoundtrip(t, p, "tu-create", "1", "Original subject")

	useLine := []byte(`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tu-update","name":"TaskUpdate","input":{"taskId":"1","status":"in_progress"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("parse update use: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tu-update","content":"Updated task #1 status"}` +
		`]},"tool_use_result":{"success":true,"taskId":"1","updatedFields":["status"],"statusChange":{"from":"pending","to":"in_progress"}}}`)
	events, err := p.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("parse update result: %v", err)
	}

	evt := pickEvent(t, events, provider.EventTaskUpdate)
	var meta provider.TaskUpdateMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.TaskID != "1" || meta.Status != "inProgress" {
		t.Errorf("meta: got %+v, want {TaskID:1 Status:inProgress}", meta)
	}
}

// TestParseTaskUpdateEmitsOwnerOnly covers the owner-claim emit:
// TaskUpdate({owner}) produces an EventTaskUpdate with the owner set
// and Status empty (meaning "leave unchanged" — triage applies the
// partial mutation).
func TestParseTaskUpdateEmitsOwnerOnly(t *testing.T) {
	p := NewParser()
	seedTaskCreateRoundtrip(t, p, "tu-create", "1", "Shared task")

	useLine := []byte(`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tu-update","name":"TaskUpdate","input":{"taskId":"1","owner":"helper-agent"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("parse update use: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tu-update","content":"Updated task #1 owner"}` +
		`]},"tool_use_result":{"success":true,"taskId":"1","updatedFields":["owner"]}}`)
	events, err := p.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("parse update result: %v", err)
	}

	evt := pickEvent(t, events, provider.EventTaskUpdate)
	var meta provider.TaskUpdateMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.Owner != "helper-agent" {
		t.Errorf("owner: got %q, want helper-agent", meta.Owner)
	}
	if meta.Status != "" {
		t.Errorf("status: got %q, want empty (leave unchanged)", meta.Status)
	}
}

// TestParseTaskUpdateEmitsDeleted covers the terminal `deleted` status:
// the parser emits EventTaskUpdate with Deleted:true.
func TestParseTaskUpdateEmitsDeleted(t *testing.T) {
	p := NewParser()
	seedTaskCreateRoundtrip(t, p, "tu-create", "1", "Doomed task")

	useLine := []byte(`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tu-update","name":"TaskUpdate","input":{"taskId":"1","status":"deleted"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("parse update use: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tu-update","content":"Updated task #1 status"}` +
		`]},"tool_use_result":{"success":true,"taskId":"1","updatedFields":["status"],"statusChange":{"from":"pending","to":"deleted"}}}`)
	events, err := p.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("parse update result: %v", err)
	}

	evt := pickEvent(t, events, provider.EventTaskUpdate)
	var meta provider.TaskUpdateMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if !meta.Deleted {
		t.Errorf("Deleted: got false, want true")
	}
}

// TestParseTaskUpdateFailureSkipsEmit covers the explicit
// failure path: a tool_result with success:false emits no event.
func TestParseTaskUpdateFailureSkipsEmit(t *testing.T) {
	p := NewParser()
	seedTaskCreateRoundtrip(t, p, "tu-create", "1", "Healthy task")

	useLine := []byte(`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tu-update","name":"TaskUpdate","input":{"taskId":"1","status":"completed"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("parse update use: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tu-update","content":"Failed"}` +
		`]},"tool_use_result":{"success":false,"taskId":"1"}}`)
	events, err := p.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("parse update result: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTaskUpdate {
			t.Errorf("expected no event on failed update, got %+v", e)
		}
	}
}

// TestParseTaskMutationResultsSuppressGenericCompletion covers the
// universal tool-lifecycle invariant carve-out: TaskCreate/TaskUpdate
// must not emit a generic EventToolComplete.
func TestParseTaskMutationResultsSuppressGenericCompletion(t *testing.T) {
	p := NewParser()
	seedTaskCreateRoundtrip(t, p, "tu-create", "1", "Some task")

	useLine := []byte(`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tu-update","name":"TaskUpdate","input":{"taskId":"1","status":"in_progress"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("use: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tu-update","content":"Updated"}` +
		`]},"tool_use_result":{"success":true,"taskId":"1"}}`)
	events, err := p.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventToolComplete && e.ItemID == "tu-update" {
			t.Errorf("TaskUpdate result must not emit EventToolComplete; got %+v", e)
		}
	}
}

// TestParseTaskListAndTaskGetAreRegularToolRows guards the explicit
// non-intercept decision for the read-only Task* tools.
func TestParseTaskListAndTaskGetAreRegularToolRows(t *testing.T) {
	cases := []struct {
		name     string
		toolName string
		input    string
	}{
		{"TaskList", "TaskList", `{}`},
		{"TaskGet", "TaskGet", `{"taskId":"1"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewParser()
			useLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
				`{"type":"tool_use","id":"tu-1","name":"` + tc.toolName + `","input":` + tc.input + `}` +
				`]}}`)
			events, err := p.ParseLine(testThread, useLine)
			if err != nil {
				t.Fatalf("parse use: %v", err)
			}
			var sawStart bool
			for _, e := range events {
				if e.Kind == provider.EventToolStart && e.ItemID == "tu-1" {
					sawStart = true
				}
			}
			if !sawStart {
				t.Errorf("%s must emit EventToolStart, got %+v", tc.toolName, events)
			}
		})
	}
}

// TestParseTaskCreateWithoutResultEmitsNothing pins the "wait for
// result" guarantee: an assistant TaskCreate with no following
// tool_result must not produce any per-task event.
func TestParseTaskCreateWithoutResultEmitsNothing(t *testing.T) {
	p := NewParser()
	useLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tu-1","name":"TaskCreate","input":{"subject":"never confirmed"}}` +
		`]}}`)
	events, err := p.ParseLine(testThread, useLine)
	if err != nil {
		t.Fatalf("parse use: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTaskCreate || e.Kind == provider.EventTaskUpdate {
			t.Errorf("expected no per-task event before tool_result; got %+v", e)
		}
	}
}

// TestParseTaskCreateMissingResultIDDropsEmit covers the
// "authoritative id required" guarantee: when tool_use_result omits
// task.id, no EventTaskCreate fires.
func TestParseTaskCreateMissingResultIDDropsEmit(t *testing.T) {
	p := NewParser()
	useLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tu-1","name":"TaskCreate","input":{"subject":"orphan"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("use: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tu-1","content":"created"}` +
		`]},"tool_use_result":{"task":{}}}`)
	events, err := p.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTaskCreate {
			t.Errorf("expected no EventTaskCreate for missing task.id, got %+v", e)
		}
	}
}

// --- helpers ---

func pickEvent(t *testing.T, events []provider.ProviderEvent, kind provider.EventKind) provider.ProviderEvent {
	t.Helper()
	for _, e := range events {
		if e.Kind == kind {
			return e
		}
	}
	t.Fatalf("no %s in events: %+v", kind, events)
	return provider.ProviderEvent{}
}

// seedTaskCreateRoundtrip runs one TaskCreate roundtrip so the parser
// records the pending mutation and emits EventTaskCreate. The events
// are consumed but not asserted — subsequent tests build on top.
func seedTaskCreateRoundtrip(t *testing.T, p *Parser, toolUseID, wireID, subject string) {
	t.Helper()
	useLine := []byte(`{"type":"assistant","message":{"id":"seed","role":"assistant","content":[` +
		`{"type":"tool_use","id":"` + toolUseID + `","name":"TaskCreate","input":{"subject":"` + subject + `"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("seed use: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"` + toolUseID + `","content":"created"}` +
		`]},"tool_use_result":{"task":{"id":"` + wireID + `","subject":"` + subject + `"}}}`)
	if _, err := p.ParseLine(testThread, resultLine); err != nil {
		t.Fatalf("seed result: %v", err)
	}
}

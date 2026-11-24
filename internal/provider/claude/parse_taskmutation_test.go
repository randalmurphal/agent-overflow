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
		if e.Kind == provider.EventToolStart || e.Kind == provider.EventTaskCreate || e.Kind == provider.EventTaskUpdate || e.Kind == provider.EventTodoUpdate {
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

// TestParseTaskUpdateEmitsMutation covers each TaskUpdate emit. The
// status row normalises to camelCase; the owner row leaves Status empty
// (meaning "leave unchanged" — triage applies the partial mutation); the
// terminal `deleted` status sets Deleted. No row emits a generic
// EventToolComplete: that is the tool-lifecycle carve-out for
// TaskCreate/TaskUpdate.
func TestParseTaskUpdateEmitsMutation(t *testing.T) {
	cases := []struct {
		name, input, result string
		want                provider.TaskUpdateMeta
	}{
		{"status", `{"taskId":"1","status":"in_progress"}`,
			`{"success":true,"taskId":"1","updatedFields":["status"],"statusChange":{"from":"pending","to":"in_progress"}}`,
			provider.TaskUpdateMeta{TaskID: "1", Status: "inProgress"}},
		{"owner only", `{"taskId":"1","owner":"helper-agent"}`,
			`{"success":true,"taskId":"1","updatedFields":["owner"]}`,
			provider.TaskUpdateMeta{TaskID: "1", Owner: "helper-agent"}},
		{"deleted", `{"taskId":"1","status":"deleted"}`,
			`{"success":true,"taskId":"1","updatedFields":["status"],"statusChange":{"from":"pending","to":"deleted"}}`,
			provider.TaskUpdateMeta{TaskID: "1", Deleted: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewParser()
			seedTaskCreateRoundtrip(t, p, "tu-create", "1", "Original subject")

			useLine := []byte(`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[` +
				`{"type":"tool_use","id":"tu-update","name":"TaskUpdate","input":` + tc.input + `}` +
				`]}}`)
			if _, err := p.ParseLine(testThread, useLine); err != nil {
				t.Fatalf("parse update use: %v", err)
			}
			resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
				`{"type":"tool_result","tool_use_id":"tu-update","content":"Updated task #1"}` +
				`]},"tool_use_result":` + tc.result + `}`)
			events, err := p.ParseLine(testThread, resultLine)
			if err != nil {
				t.Fatalf("parse update result: %v", err)
			}

			var meta provider.TaskUpdateMeta
			if err := json.Unmarshal(pickEvent(t, events, provider.EventTaskUpdate).Meta, &meta); err != nil {
				t.Fatalf("unmarshal meta: %v", err)
			}
			if meta != tc.want {
				t.Errorf("meta: got %+v, want %+v", meta, tc.want)
			}
			for _, e := range events {
				if e.Kind == provider.EventToolComplete && e.ItemID == "tu-update" {
					t.Errorf("TaskUpdate result must not emit EventToolComplete; got %+v", e)
				}
			}
		})
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

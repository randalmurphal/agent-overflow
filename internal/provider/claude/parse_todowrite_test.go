package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// TestParseAssistantTodoWriteEmitsTodoUpdate covers the parse-side
// reroute for TodoWrite: the tool_use must produce exactly one
// EventTodoUpdate (no generic EventToolStart), with a normalized plan
// payload on Meta and the camelCase status enum.
func TestParseAssistantTodoWriteEmitsTodoUpdate(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tool-todo-1","name":"TodoWrite","input":{"todos":[` +
		`{"content":"Refactor parser","status":"in_progress","activeForm":"Refactoring parser"},` +
		`{"content":"Update fixtures","status":"pending","activeForm":"Updating fixtures"},` +
		`{"content":"Wire UI panel","status":"completed","activeForm":"Wiring UI panel"}` +
		`]}}` +
		`]}}`)

	p := NewParser()
	events, err := p.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}

	evt := events[0]
	if evt.Kind != provider.EventTodoUpdate {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventTodoUpdate)
	}
	if evt.ItemID != "tool-todo-1" {
		t.Errorf("item id: got %q, want tool-todo-1", evt.ItemID)
	}
	if evt.ItemType != "TodoWrite" {
		t.Errorf("item type: got %q, want TodoWrite", evt.ItemType)
	}

	var meta struct {
		Kind  string `json:"kind"`
		Title string `json:"title"`
		Plan  []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.Kind != "todo_update" {
		t.Errorf("meta.kind: got %q, want todo_update", meta.Kind)
	}
	if meta.Title != "Updated Todos" {
		t.Errorf("meta.title: got %q, want Updated Todos", meta.Title)
	}
	if len(meta.Plan) != 3 {
		t.Fatalf("plan steps: got %d, want 3", len(meta.Plan))
	}

	// Order preserved from input; status normalized in_progress -> inProgress.
	want := []struct{ step, status string }{
		{"Refactor parser", "inProgress"},
		{"Update fixtures", "pending"},
		{"Wire UI panel", "completed"},
	}
	for i, w := range want {
		if meta.Plan[i].Step != w.step {
			t.Errorf("plan[%d].step: got %q, want %q", i, meta.Plan[i].Step, w.step)
		}
		if meta.Plan[i].Status != w.status {
			t.Errorf("plan[%d].status: got %q, want %q", i, meta.Plan[i].Status, w.status)
		}
	}

	// Marking is recorded so the matching tool_result is dropped.
	if !p.isTodoWrite("tool-todo-1") {
		t.Errorf("tool_use_id was not marked as TodoWrite")
	}
}

// TestParseAssistantTodoWriteEmptyTodosDropsEvent covers the empty-input
// guard: an empty plan must not produce a frontend event the UI would
// render as "no plan".
func TestParseAssistantTodoWriteEmptyTodosDropsEvent(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tool-todo-empty","name":"TodoWrite","input":{"todos":[]}}` +
		`]}}`)

	p := NewParser()
	events, err := p.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTodoUpdate || e.Kind == provider.EventToolStart {
			t.Fatalf("expected no plan/tool events for empty todos, got %+v", e)
		}
	}
	if p.isTodoWrite("tool-todo-empty") {
		t.Errorf("empty input should not mark the tool_use_id")
	}
}

// TestParseUserTodoWriteToolResultIsDropped covers the parse-side drop
// of the matching tool_result: because no EventToolStart was emitted
// for TodoWrite, the completion would be an orphan and is suppressed.
func TestParseUserTodoWriteToolResultIsDropped(t *testing.T) {
	p := NewParser()

	// First feed the tool_use line so the parser marks the id.
	useLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tool-todo-2","name":"TodoWrite","input":{"todos":[` +
		`{"content":"Step one","status":"pending","activeForm":"Doing step one"}` +
		`]}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("parse use: %v", err)
	}

	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tool-todo-2","content":"Todos have been modified successfully."}` +
		`]}}`)
	events, err := p.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventToolComplete {
			t.Fatalf("tool_result for TodoWrite must not emit EventToolComplete; got %+v", e)
		}
	}
	if p.isTodoWrite("tool-todo-2") {
		t.Errorf("flag should be cleared after the matching tool_result")
	}
}

// TestExtractTodoWriteStepsFiltersAndNormalizes pins the helper
// behaviour the parser depends on: whitespace-only content is filtered,
// status enum is normalised, unknown statuses default to pending so
// the frontend always has a renderable bucket. Runs as a small table
// directly against the helpers so a regression in the input-shape
// handling surfaces here, not in a ParseLine integration test where
// the wire envelope obscures the failure.
func TestExtractTodoWriteStepsFiltersAndNormalizes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []todoWriteStep
	}{
		{
			name: "happy path normalises in_progress to inProgress",
			in:   `{"todos":[{"content":"Refactor parser","status":"in_progress","activeForm":""}]}`,
			want: []todoWriteStep{{Step: "Refactor parser", Status: "inProgress"}},
		},
		{
			name: "whitespace-only content is filtered",
			in:   `{"todos":[{"content":"   ","status":"pending"},{"content":"real","status":"pending"}]}`,
			want: []todoWriteStep{{Step: "real", Status: "pending"}},
		},
		{
			name: "missing todos key returns nil",
			in:   `{}`,
			want: nil,
		},
		{
			name: "garbage json returns nil",
			in:   `not json`,
			want: nil,
		},
		{
			name: "unknown status defaults to pending",
			in:   `{"todos":[{"content":"do thing","status":"frobnicating"}]}`,
			want: []todoWriteStep{{Step: "do thing", Status: "pending"}},
		},
		{
			name: "empty status defaults to pending",
			in:   `{"todos":[{"content":"do thing","status":""}]}`,
			want: []todoWriteStep{{Step: "do thing", Status: "pending"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractTodoWriteSteps(json.RawMessage(tc.in))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d steps, want %d (got=%+v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("step[%d]: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestParseUserNonTodoWriteToolResultStillCompletes guards against the
// carve-out leaking: a normal tool_result still emits its
// EventToolComplete even when the parser has TodoWrite state for a
// different tool_use_id.
func TestParseUserNonTodoWriteToolResultStillCompletes(t *testing.T) {
	p := NewParser()
	p.markTodoWrite("tool-todo-3")

	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tool-other-1","content":"ok"}` +
		`]}}`)
	events, err := p.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var sawComplete bool
	for _, e := range events {
		if e.Kind == provider.EventToolComplete && e.ItemID == "tool-other-1" {
			sawComplete = true
		}
	}
	if !sawComplete {
		t.Errorf("expected EventToolComplete for tool-other-1, got %+v", events)
	}
	// And the unrelated TodoWrite mark stays in place.
	if !p.isTodoWrite("tool-todo-3") {
		t.Errorf("TodoWrite mark for an unrelated id was incorrectly cleared")
	}
}

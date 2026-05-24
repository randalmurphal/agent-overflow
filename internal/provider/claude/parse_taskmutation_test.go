package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// TestParseAssistantTaskCreateNoEventUntilResult covers the staged
// behaviour: the assistant tool_use for TaskCreate must not emit
// either EventToolStart or EventTodoUpdate. The snapshot only fires
// after the matching tool_result confirms the create with an
// authoritative id.
func TestParseAssistantTaskCreateNoEventUntilResult(t *testing.T) {
	p := NewParser()

	useLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tool-create-1","name":"TaskCreate","input":{"subject":"Fix lifespan leak","description":"Widen the try block","activeForm":"Fixing lifespan leak"}}` +
		`]}}`)
	events, err := p.ParseLine(testThread, useLine)
	if err != nil {
		t.Fatalf("parse use: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventToolStart || e.Kind == provider.EventTodoUpdate {
			t.Fatalf("expected no tool/todo event on TaskCreate use; got %+v", e)
		}
	}
}

// TestParseTaskCreateEmitsSnapshotOnResult covers the happy-path
// roundtrip: assistant tool_use stages, tool_result applies the
// mutation using the wire-assigned id, snapshot fires through the
// EventTodoUpdate channel.
func TestParseTaskCreateEmitsSnapshotOnResult(t *testing.T) {
	p := NewParser()

	useLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tool-create-1","name":"TaskCreate","input":{"subject":"Fix lifespan leak","description":"Widen the try block","activeForm":"Fixing lifespan leak"}}` +
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

	var snapshot *provider.ProviderEvent
	for i := range events {
		if events[i].Kind == provider.EventTodoUpdate {
			snapshot = &events[i]
		}
		if events[i].Kind == provider.EventToolComplete && events[i].ItemID == "tool-create-1" {
			t.Errorf("TaskCreate must not emit EventToolComplete; got %+v", events[i])
		}
	}
	if snapshot == nil {
		t.Fatalf("expected EventTodoUpdate snapshot, got %+v", events)
	}
	if snapshot.ItemID != "tool-create-1" {
		t.Errorf("snapshot itemID: got %q, want tool-create-1", snapshot.ItemID)
	}

	plan := decodeSnapshotPlan(t, snapshot.Meta)
	if len(plan) != 1 {
		t.Fatalf("plan length: got %d, want 1: %+v", len(plan), plan)
	}
	if plan[0].ID != "1" || plan[0].Step != "Fix lifespan leak" || plan[0].Status != "pending" {
		t.Errorf("plan[0]: got %+v, want {id:1 step:Fix lifespan leak status:pending}", plan[0])
	}
	if plan[0].Owner != "" {
		t.Errorf("plan[0].owner: got %q, want empty", plan[0].Owner)
	}
}

// TestParseTaskCreateSequentialIdsPreserveOrder covers the
// accumulating-snapshot semantics: three TaskCreate roundtrips
// produce a three-item snapshot in insertion order, each carrying
// the wire-assigned id.
func TestParseTaskCreateSequentialIdsPreserveOrder(t *testing.T) {
	p := NewParser()
	feed := func(t *testing.T, useLine, resultLine string) {
		t.Helper()
		if _, err := p.ParseLine(testThread, []byte(useLine)); err != nil {
			t.Fatalf("use: %v", err)
		}
		if _, err := p.ParseLine(testThread, []byte(resultLine)); err != nil {
			t.Fatalf("result: %v", err)
		}
	}

	feed(t,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tu-1","name":"TaskCreate","input":{"subject":"first","description":"d","activeForm":"a"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-1","content":"ok"}]},"tool_use_result":{"task":{"id":"1","subject":"first"}}}`,
	)
	feed(t,
		`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[{"type":"tool_use","id":"tu-2","name":"TaskCreate","input":{"subject":"second","description":"d","activeForm":"a"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-2","content":"ok"}]},"tool_use_result":{"task":{"id":"2","subject":"second"}}}`,
	)
	useLine := `{"type":"assistant","message":{"id":"msg-3","role":"assistant","content":[{"type":"tool_use","id":"tu-3","name":"TaskCreate","input":{"subject":"third","description":"d","activeForm":"a"}}]}}`
	resultLine := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-3","content":"ok"}]},"tool_use_result":{"task":{"id":"3","subject":"third"}}}`
	if _, err := p.ParseLine(testThread, []byte(useLine)); err != nil {
		t.Fatalf("use 3: %v", err)
	}
	events, err := p.ParseLine(testThread, []byte(resultLine))
	if err != nil {
		t.Fatalf("result 3: %v", err)
	}

	snapshot := pickSnapshot(t, events)
	plan := decodeSnapshotPlan(t, snapshot.Meta)
	if len(plan) != 3 {
		t.Fatalf("plan length: got %d, want 3", len(plan))
	}
	want := []struct{ id, step string }{
		{"1", "first"}, {"2", "second"}, {"3", "third"},
	}
	for i, w := range want {
		if plan[i].ID != w.id || plan[i].Step != w.step {
			t.Errorf("plan[%d]: got %+v, want {id:%s step:%s}", i, plan[i], w.id, w.step)
		}
	}
}

// TestParseTaskUpdateMutatesStatus covers the status-transition
// snapshot: a TaskUpdate with status:"in_progress" rewrites just that
// task's status, leaves subject untouched, and uses the camelCase
// `inProgress` enum on the wire.
func TestParseTaskUpdateMutatesStatus(t *testing.T) {
	p := NewParser()
	seedSingleTask(t, p, "tu-create", "1", "Original subject")

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

	snapshot := pickSnapshot(t, events)
	plan := decodeSnapshotPlan(t, snapshot.Meta)
	if len(plan) != 1 {
		t.Fatalf("plan length: got %d, want 1", len(plan))
	}
	if plan[0].ID != "1" || plan[0].Step != "Original subject" || plan[0].Status != "inProgress" {
		t.Errorf("plan[0]: got %+v, want {id:1 step:Original subject status:inProgress}", plan[0])
	}
}

// TestParseTaskUpdateOwnerEmitsBadgeField covers the owner-claim
// path: TaskUpdate({owner}) stamps the owner onto the snapshot so
// the frontend can render the subagent badge. The companion
// status-preservation check is critical — earlier the normaliser
// returned "pending" for an empty input string, so an owner-only
// update would silently clobber the existing task status. The
// snapshot must reflect ONLY the field the caller updated.
func TestParseTaskUpdateOwnerEmitsBadgeField(t *testing.T) {
	p := NewParser()
	seedSingleTask(t, p, "tu-create", "1", "Shared task")

	// Move the seeded task to in_progress so the next update can be
	// checked against a non-default starting status.
	if !p.mutateTask("1", "", "", "inProgress", false) {
		t.Fatalf("mutateTask seed: failed to advance status to inProgress")
	}

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

	snapshot := pickSnapshot(t, events)
	plan := decodeSnapshotPlan(t, snapshot.Meta)
	if len(plan) != 1 {
		t.Fatalf("plan length: got %d, want 1", len(plan))
	}
	if plan[0].Owner != "helper-agent" {
		t.Errorf("plan[0].owner: got %q, want helper-agent", plan[0].Owner)
	}
	if plan[0].Status != "inProgress" {
		t.Errorf("plan[0].status: got %q, want inProgress (preserved)", plan[0].Status)
	}
	if plan[0].Step != "Shared task" {
		t.Errorf("plan[0].step: got %q, want Shared task (preserved)", plan[0].Step)
	}
}

// TestParseTaskUpdateDeletedRemovesOnlyEntry pins the parser's
// empty-snapshot drop: deleting the only task leaves a zero-task
// state, which the parser does NOT emit (matches TodoWrite's empty
// drop and triage's handleTodoUpdate ignore-empty rule). The
// taskOrder and tasksByID state still mutate so a subsequent
// TaskCreate starts from a clean slate.
//
// Trade-off: the activity rail keeps showing the deleted task until
// the next mutation refreshes it, mirroring the legacy TodoWrite
// behaviour. The delete-middle-of-N test covers the case where the
// snapshot DOES emit because non-deleted tasks survive.
func TestParseTaskUpdateDeletedRemovesOnlyEntry(t *testing.T) {
	p := NewParser()
	seedSingleTask(t, p, "tu-create", "1", "Doomed task")

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

	for _, e := range events {
		if e.Kind == provider.EventTodoUpdate {
			t.Errorf("expected no snapshot for empty post-delete state (matches TodoWrite empty drop); got %+v", e)
		}
	}
	if got := p.taskSnapshot(); got != nil {
		t.Errorf("taskSnapshot: state should be empty after deleting the only task; got %+v", got)
	}
}

// TestParseTaskUpdateFailureSkipsSnapshot covers the explicit
// failure path: a tool_result with success:false leaves state
// untouched and emits no snapshot.
func TestParseTaskUpdateFailureSkipsSnapshot(t *testing.T) {
	p := NewParser()
	seedSingleTask(t, p, "tu-create", "1", "Healthy task")

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
		if e.Kind == provider.EventTodoUpdate {
			t.Errorf("expected no snapshot on failed update, got %+v", e)
		}
	}
}

// TestParseTaskMutationResultsSuppressGenericCompletion covers the
// universal tool-lifecycle invariant carve-out: because the
// assistant side never emitted EventToolStart for TaskCreate /
// TaskUpdate, the matching result must not emit a generic
// EventToolComplete either. Mirrors the TodoWrite test of the same
// shape.
func TestParseTaskMutationResultsSuppressGenericCompletion(t *testing.T) {
	p := NewParser()
	seedSingleTask(t, p, "tu-create", "1", "Some task")

	// Update result for a known task — successful path.
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

// TestParseTaskUpdateUnknownIDIsNoop guards against silently
// corrupting state when Claude (or a hostile wire) issues a
// TaskUpdate against an id we have never seen.
func TestParseTaskUpdateUnknownIDIsNoop(t *testing.T) {
	p := NewParser()

	useLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tu-update","name":"TaskUpdate","input":{"taskId":"99","status":"completed"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("use: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tu-update","content":"Updated"}` +
		`]},"tool_use_result":{"success":true,"taskId":"99","updatedFields":["status"]}}`)
	events, err := p.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTodoUpdate {
			t.Errorf("expected no snapshot for unknown taskId, got %+v", e)
		}
	}
}

// TestParseTaskListAndTaskGetAreRegularToolRows guards the explicit
// non-intercept decision for the read-only Task* tools: they MUST
// emit standard EventToolStart + EventToolComplete so the user sees
// them in the chat timeline.
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
// tool_result must leave the parser's task state empty so a hostile
// or crashed roundtrip cannot populate the snapshot speculatively.
// Verified observably by issuing a follow-up TaskUpdate against the
// id the staged create would have produced and asserting no snapshot
// fires (the update sees an empty mirror and treats the id as
// unknown).
func TestParseTaskCreateWithoutResultEmitsNothing(t *testing.T) {
	p := NewParser()
	useLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tu-1","name":"TaskCreate","input":{"subject":"never confirmed"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("parse use: %v", err)
	}

	probeUse := []byte(`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tu-2","name":"TaskUpdate","input":{"taskId":"1","status":"completed"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, probeUse); err != nil {
		t.Fatalf("probe use: %v", err)
	}
	probeResult := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tu-2","content":"ok"}` +
		`]},"tool_use_result":{"success":true,"taskId":"1"}}`)
	events, err := p.ParseLine(testThread, probeResult)
	if err != nil {
		t.Fatalf("probe result: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTodoUpdate {
			t.Errorf("expected no snapshot — staged create was never confirmed, so TaskUpdate(taskId:\"1\") must see an unknown id; got %+v", e)
		}
	}
}

// TestParseTaskUpdateDeletedRemovesMiddleEntry covers the taskOrder
// splice correctness path: deleting one task out of N must preserve
// the surviving tasks' order so subsequent snapshots remain stable.
// Insertion-order matters because the activity rail relies on it for
// the secondary sort within the same status bucket.
func TestParseTaskUpdateDeletedRemovesMiddleEntry(t *testing.T) {
	p := NewParser()
	seedSingleTask(t, p, "create-1", "1", "first")
	seedSingleTask(t, p, "create-2", "2", "second")
	seedSingleTask(t, p, "create-3", "3", "third")

	useLine := []byte(`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[` +
		`{"type":"tool_use","id":"del-2","name":"TaskUpdate","input":{"taskId":"2","status":"deleted"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("parse update use: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"del-2","content":"ok"}` +
		`]},"tool_use_result":{"success":true,"taskId":"2","statusChange":{"from":"pending","to":"deleted"}}}`)
	events, err := p.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("parse update result: %v", err)
	}

	snapshot := pickSnapshot(t, events)
	plan := decodeSnapshotPlan(t, snapshot.Meta)
	if len(plan) != 2 {
		t.Fatalf("plan length: got %d, want 2", len(plan))
	}
	if plan[0].ID != "1" || plan[1].ID != "3" {
		t.Errorf("plan order: got [%s, %s], want [1, 3]", plan[0].ID, plan[1].ID)
	}
}

// TestParseTaskUpdateNoOpInputPreservesAllFields pins the contract
// that a TaskUpdate carrying nothing but taskId leaves every field
// untouched. The owner-only regression test covers the partial-update
// path; this is the symmetric guard for the empty-input case so any
// future change to decodeTaskUpdateInput or mutateTask can't silently
// blank a task's fields.
func TestParseTaskUpdateNoOpInputPreservesAllFields(t *testing.T) {
	p := NewParser()
	seedSingleTask(t, p, "tu-create", "1", "Original task")
	if !p.mutateTask("1", "", "claimant", "completed", false) {
		t.Fatalf("mutateTask seed: failed to apply non-default fields")
	}

	useLine := []byte(`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[` +
		`{"type":"tool_use","id":"noop","name":"TaskUpdate","input":{"taskId":"1"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("parse update use: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"noop","content":"ok"}` +
		`]},"tool_use_result":{"success":true,"taskId":"1"}}`)
	events, err := p.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("parse update result: %v", err)
	}

	snapshot := pickSnapshot(t, events)
	plan := decodeSnapshotPlan(t, snapshot.Meta)
	if len(plan) != 1 {
		t.Fatalf("plan length: got %d, want 1", len(plan))
	}
	if plan[0].Step != "Original task" || plan[0].Status != "completed" || plan[0].Owner != "claimant" {
		t.Errorf("plan[0]: got %+v, want {step:Original task status:completed owner:claimant}", plan[0])
	}
}

// TestParseTaskMutationCrossSessionIsolation pins the per-parser
// scoping: two NewParser() instances must not share task state. Cheap
// guard against any future regression that lifts the maps to a
// package-level cache.
func TestParseTaskMutationCrossSessionIsolation(t *testing.T) {
	a := NewParser()
	b := NewParser()
	seedSingleTask(t, a, "tu-1", "1", "Only in A")

	if got := b.taskSnapshot(); got != nil {
		t.Errorf("parser B leaked tasks from parser A: %+v", got)
	}
	// Also assert observably: a TaskUpdate against parser B for the id
	// parser A holds must produce no snapshot.
	useLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tu-2","name":"TaskUpdate","input":{"taskId":"1","status":"completed"}}` +
		`]}}`)
	if _, err := b.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("parser B use: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tu-2","content":"ok"}` +
		`]},"tool_use_result":{"success":true,"taskId":"1"}}`)
	events, err := b.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("parser B result: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTodoUpdate {
			t.Errorf("parser B emitted snapshot for an id parser A owns: %+v", e)
		}
	}
}

// TestParseTaskCreateMissingResultIDDropsMutation covers the
// "authoritative id required" guarantee: when the tool_use_result
// omits task.id (or fails to decode), the create is dropped so the
// parser cannot key its local mirror on a guess.
func TestParseTaskCreateMissingResultIDDropsMutation(t *testing.T) {
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
		if e.Kind == provider.EventTodoUpdate {
			t.Errorf("expected no snapshot for missing task.id, got %+v", e)
		}
	}
	if got := p.taskSnapshot(); got != nil {
		t.Errorf("taskSnapshot: state populated despite missing id; got %+v", got)
	}
}

// TestParseTaskCreateDuplicateIDPreservesStatus covers the
// defensive duplicate-id branch in upsertTaskFromCreate. If the wire
// shape ever delivers two TaskCreate results with the same task.id —
// out-of-order replay, hostile wire — the prior status from any
// TaskUpdate must survive. Pinning this stops the historical
// regression where applyTaskCreateResult passed status:"pending" and
// silently clobbered an inProgress/completed task.
func TestParseTaskCreateDuplicateIDPreservesStatus(t *testing.T) {
	p := NewParser()
	seedSingleTask(t, p, "first-create", "1", "Original")
	if !p.mutateTask("1", "", "", "inProgress", false) {
		t.Fatalf("mutateTask seed: failed to advance status to inProgress")
	}

	useLine := []byte(`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[` +
		`{"type":"tool_use","id":"dup-create","name":"TaskCreate","input":{"subject":"Replayed subject"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("dup use: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"dup-create","content":"ok"}` +
		`]},"tool_use_result":{"task":{"id":"1","subject":"Replayed subject"}}}`)
	if _, err := p.ParseLine(testThread, resultLine); err != nil {
		t.Fatalf("dup result: %v", err)
	}

	got := p.taskSnapshot()
	if len(got) != 1 {
		t.Fatalf("expected one task after duplicate id, got %+v", got)
	}
	if got[0].Status != "inProgress" {
		t.Errorf("status: got %q, want inProgress (preserved through duplicate create)", got[0].Status)
	}
	if got[0].Subject != "Replayed subject" {
		t.Errorf("subject: got %q, want Replayed subject", got[0].Subject)
	}
}

// TestParseTaskCreateSnapshotEventCarriesToolName guards that
// EventTodoUpdate emitted by the Task* path sets ItemType to the
// originating tool name (sibling to appendTodoWriteEvent's
// ItemType:"TodoWrite"). Without this, triage and downstream
// consumers can't tell which producer drove the snapshot.
func TestParseTaskCreateSnapshotEventCarriesToolName(t *testing.T) {
	p := NewParser()
	useLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tu-1","name":"TaskCreate","input":{"subject":"hello"}}` +
		`]}}`)
	if _, err := p.ParseLine(testThread, useLine); err != nil {
		t.Fatalf("use: %v", err)
	}
	resultLine := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tu-1","content":"ok"}` +
		`]},"tool_use_result":{"task":{"id":"1","subject":"hello"}}}`)
	events, err := p.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	snapshot := pickSnapshot(t, events)
	if snapshot.ItemType != "TaskCreate" {
		t.Errorf("ItemType: got %q, want TaskCreate", snapshot.ItemType)
	}
}

// --- helpers ---

type snapshotStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
	ID     string `json:"id"`
	Owner  string `json:"owner"`
}

func decodeSnapshotPlan(t *testing.T, meta json.RawMessage) []snapshotStep {
	t.Helper()
	var decoded struct {
		Plan []snapshotStep `json:"plan"`
	}
	if err := json.Unmarshal(meta, &decoded); err != nil {
		t.Fatalf("unmarshal snapshot meta: %v", err)
	}
	return decoded.Plan
}

func pickSnapshot(t *testing.T, events []provider.ProviderEvent) provider.ProviderEvent {
	t.Helper()
	for _, e := range events {
		if e.Kind == provider.EventTodoUpdate {
			return e
		}
	}
	t.Fatalf("no EventTodoUpdate in events: %+v", events)
	return provider.ProviderEvent{}
}

// seedSingleTask runs one TaskCreate roundtrip so the parser holds a
// known task with id "1" the subsequent TaskUpdate cases can mutate.
func seedSingleTask(t *testing.T, p *Parser, toolUseID, wireID, subject string) {
	t.Helper()
	useLine := []byte(`{"type":"assistant","message":{"id":"seed","role":"assistant","content":[` +
		`{"type":"tool_use","id":"` + toolUseID + `","name":"TaskCreate","input":{"subject":"` + subject + `","description":"d","activeForm":"a"}}` +
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

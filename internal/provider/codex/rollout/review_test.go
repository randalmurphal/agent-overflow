package rollout

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"

	_ "modernc.org/sqlite"
)

const reviewOuterTurnID = "019fdd9f-c8fd-7093-8266-8af5e196dd01"
const reviewControlTurnID = "019fdd9f-c8fd-7093-8266-8af5e196dd02"
const reviewChildThreadID = "019fdd9f-c8fd-7093-8266-8af5e196dd03"

func reviewRootLines() []string {
	return []string{
		metaLine,
		`{"timestamp":"2026-08-07T19:07:45.000Z","type":"event_msg","payload":{"type":"entered_review_mode","target":{"type":"custom","instructions":"Inspect the change."},"user_facing_hint":"Inspect the change.","turn_id":"` + reviewOuterTurnID + `","item_id":"review-enter"}}`,
		`{"timestamp":"2026-08-07T19:07:46.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + reviewControlTurnID + `","started_at":1786133866,"model_context_window":258400}}`,
		`{"timestamp":"2026-08-07T19:07:57.000Z","type":"response_item","payload":{"type":"message","id":"review-context","role":"user","content":[{"type":"input_text","text":"<user_action>formatted review context</user_action>"}]}}`,
		`{"timestamp":"2026-08-07T19:07:58.000Z","type":"event_msg","payload":{"type":"exited_review_mode","turn_id":"` + reviewOuterTurnID + `","item_id":"review-exit","review_output":{"findings":[],"overall_correctness":"patch is correct","overall_explanation":"No issues found."}}}`,
		`{"timestamp":"2026-08-07T19:07:59.000Z","type":"event_msg","payload":{"type":"agent_message","message":"No issues found.","phase":null}}`,
		`{"timestamp":"2026-08-07T19:08:00.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + reviewOuterTurnID + `","started_at":1786133866,"completed_at":1786133880}}`,
	}
}

func TestParseProjectsReviewAsOneAgentLaunchAndSourcedResult(t *testing.T) {
	result := parseFixture(t, writeRollout(t, testSessionID, reviewRootLines()...))
	want := []provider.EventKind{
		provider.EventTurnStart,
		provider.EventUserText,
		provider.EventToolStart,
		provider.EventContentBlockStop,
		provider.EventToolComplete,
		provider.EventCommandResult,
		provider.EventTurnComplete,
	}
	if got := kinds(result.Events); len(got) != len(want) {
		t.Fatalf("review events = %v, want %v", got, want)
	}
	for i := range want {
		if result.Events[i].Kind != want[i] {
			t.Fatalf("event %d = %s, want %s", i, result.Events[i].Kind, want[i])
		}
		if result.Events[i].TurnID != reviewOuterTurnID {
			t.Errorf("event %d turn = %q, want outer review turn", i, result.Events[i].TurnID)
		}
	}
	if result.Events[1].Content != "/review custom Inspect the change." {
		t.Fatalf("review command = %q", result.Events[1].Content)
	}
	launch := result.Events[2]
	if launch.ItemType != importedCodexReviewToolName {
		t.Fatalf("launch = %+v", launch.ProviderEvent)
	}
	var launchMeta struct {
		Input struct {
			ControlTurnID string `json:"reviewControlTurnId"`
		} `json:"input"`
	}
	if err := json.Unmarshal(launch.Meta, &launchMeta); err != nil {
		t.Fatal(err)
	}
	if launchMeta.Input.ControlTurnID != reviewControlTurnID {
		t.Fatalf("control turn = %q", launchMeta.Input.ControlTurnID)
	}
	if result.Events[3].ParentToolUseID != launch.ItemID || result.Events[3].Content != "No issues found." {
		t.Fatalf("nested result = %+v", result.Events[3].ProviderEvent)
	}
	var resultMeta provider.CommandResultMeta
	if err := json.Unmarshal(result.Events[5].Meta, &resultMeta); err != nil {
		t.Fatal(err)
	}
	if resultMeta.AgentResult == nil || resultMeta.AgentResult.LaunchID != launch.ItemID {
		t.Fatalf("command result meta = %+v", resultMeta)
	}
}

func TestParseDoesNotAdvancePastAnIncompleteReview(t *testing.T) {
	lines := reviewRootLines()[:3]
	path := writeRollout(t, testSessionID, lines...)
	result := parseFixture(t, path)
	if len(result.Events) != 0 {
		t.Fatalf("incomplete review leaked events: %v", kinds(result.Events))
	}
	wantOffset := int64(len(metaLine) + 1)
	if result.EndOffset != wantOffset {
		t.Fatalf("end offset = %d, want review boundary %d", result.EndOffset, wantOffset)
	}
}

func TestParseReviewFailureWithoutAResultStaysFailed(t *testing.T) {
	lines := append([]string{}, reviewRootLines()[:3]...)
	lines = append(lines,
		`{"timestamp":"2026-08-07T19:07:58.000Z","type":"event_msg","payload":{"type":"error","message":"review worker failed"}}`,
		`{"timestamp":"2026-08-07T19:08:00.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"`+reviewOuterTurnID+`","started_at":1786133866,"completed_at":1786133880}}`,
	)
	result := parseFixture(t, writeRollout(t, testSessionID, lines...))
	var toolComplete, commandResult, turnComplete *provider.ProviderEvent
	for i := range result.Events {
		event := &result.Events[i].ProviderEvent
		switch event.Kind {
		case provider.EventToolComplete:
			toolComplete = event
		case provider.EventCommandResult:
			commandResult = event
		case provider.EventTurnComplete:
			turnComplete = event
		}
	}
	if toolComplete == nil || !strings.Contains(string(toolComplete.Meta), `"item_status":"failed"`) {
		t.Fatalf("failed review tool completion = %+v", toolComplete)
	}
	if commandResult == nil || commandResult.Content != "Code review failed: review worker failed" {
		t.Fatalf("failed review result = %+v", commandResult)
	}
	if turnComplete == nil {
		t.Fatal("failed review has no logical turn completion")
	}
	complete := turnComplete.TurnComplete.(*provider.WireTurnCompleteMeta)
	if complete.StopReason != "error" || complete.ErrorMessage != "review worker failed" {
		t.Fatalf("failed review completion = %+v", complete)
	}
}

func TestParseReviewResultWinsOverAnInternalError(t *testing.T) {
	base := reviewRootLines()
	lines := append([]string{}, base[:4]...)
	lines = append(lines, `{"timestamp":"2026-08-07T19:07:57.500Z","type":"event_msg","payload":{"type":"error","message":"report tool failed"}}`)
	lines = append(lines, base[4:]...)
	result := parseFixture(t, writeRollout(t, testSessionID, lines...))
	for i := range result.Events {
		event := result.Events[i].ProviderEvent
		switch event.Kind {
		case provider.EventToolComplete:
			if !strings.Contains(string(event.Meta), `"item_status":"completed"`) {
				t.Fatalf("review with a result was marked failed: %s", event.Meta)
			}
		case provider.EventTurnComplete:
			complete := event.TurnComplete.(*provider.WireTurnCompleteMeta)
			if complete.StopReason != "end_turn" {
				t.Fatalf("review with a result completion = %+v", complete)
			}
		}
	}
}

func TestProjectReviewChildrenNestsChildActivityAndUsesReviewModel(t *testing.T) {
	home := t.TempDir()
	rootPath := writeRollout(t, testSessionID, reviewRootLines()...)
	root := parseFixture(t, rootPath)
	childPath := filepath.Join(home, "sessions", "2026", "08", "07", "rollout-2026-08-07T15-07-44-"+reviewChildThreadID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(childPath), 0o755); err != nil {
		t.Fatal(err)
	}
	childLines := []string{
		`{"timestamp":"2026-08-07T19:07:46.000Z","type":"session_meta","payload":{"id":"` + reviewChildThreadID + `","parent_thread_id":"` + testSessionID + `","thread_source":"subagent","source":{"subagent":"review"},"cwd":"/repo","history_mode":"legacy"}}`,
		`{"timestamp":"2026-08-07T19:07:46.100Z","type":"turn_context","payload":{"turn_id":"` + reviewControlTurnID + `","cwd":"/repo","model":"gpt-review","effort":"high"}}`,
		`{"timestamp":"2026-08-07T19:07:46.200Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + reviewControlTurnID + `","started_at":1786133866,"model_context_window":258400}}`,
		`{"timestamp":"2026-08-07T19:07:47.000Z","type":"event_msg","payload":{"type":"user_message","message":"Inspect the change."}}`,
		`{"timestamp":"2026-08-07T19:07:48.000Z","type":"response_item","payload":{"type":"function_call","call_id":"call-1","name":"exec_command","arguments":"{\"cmd\":\"git diff\"}"}}`,
		`{"timestamp":"2026-08-07T19:07:49.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"diff output"}}`,
		`{"timestamp":"2026-08-07T19:07:50.000Z","type":"event_msg","payload":{"type":"agent_message","message":"{\"findings\":[]}","phase":null}}`,
		`{"timestamp":"2026-08-07T19:07:51.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + reviewControlTurnID + `","started_at":1786133866,"completed_at":1786133871}}`,
	}
	if err := os.WriteFile(childPath, []byte(joinLines(childLines)), 0o644); err != nil {
		t.Fatal(err)
	}
	writeReviewStateDB(t, home, childPath)

	projected := ProjectReviewChildren(context.Background(), home, testSessionID, root)
	launchIndex := -1
	for i, event := range projected.Events {
		if event.Kind == provider.EventToolStart && event.ItemType == importedCodexReviewToolName {
			launchIndex = i
			var meta struct {
				Input struct {
					Model string `json:"model"`
				} `json:"input"`
			}
			if err := json.Unmarshal(event.Meta, &meta); err != nil {
				t.Fatal(err)
			}
			if meta.Input.Model != "gpt-review" {
				t.Fatalf("review model = %q", meta.Input.Model)
			}
			break
		}
	}
	if launchIndex < 0 || launchIndex+1 >= len(projected.Events) {
		t.Fatal("projected review launch missing")
	}
	childTool := projected.Events[launchIndex+1]
	if childTool.Kind != provider.EventToolStart || childTool.ParentToolUseID != projected.Events[launchIndex].ItemID {
		t.Fatalf("projected child activity = %+v", childTool.ProviderEvent)
	}
	if childTool.SourceOffset != 0 || childTool.SourceUUID == "" {
		t.Fatalf("child source coordinates = %q/%d", childTool.SourceUUID, childTool.SourceOffset)
	}
	for _, event := range projected.Events {
		if event.Content == `{"findings":[]}` {
			t.Fatal("raw reviewer JSON leaked into the projected transcript")
		}
	}
}

func writeReviewStateDB(t *testing.T, home, childPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, StateDBName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE threads (
id TEXT PRIMARY KEY, rollout_path TEXT, model TEXT, reasoning_effort TEXT,
source TEXT, created_at INTEGER, created_at_ms INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO threads
(id, rollout_path, model, reasoning_effort, source, created_at, created_at_ms)
VALUES (?, ?, 'index-fallback', 'high', '{"subagent":"review"}', 1, 1000)`, reviewChildThreadID, childPath); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func joinLines(lines []string) string {
	var out string
	for _, line := range lines {
		out += line + "\n"
	}
	return out
}

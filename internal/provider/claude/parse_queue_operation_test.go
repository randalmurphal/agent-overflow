package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

const (
	droppedWakeFixture = "../../../docs/references/fixtures/claude/local_agent_dropped_wake_20261014.ndjson"
	mirrorWakeFixture  = "../../../docs/references/fixtures/claude/local_agent_mirror_wake_20261014.ndjson"
)

func wakeDroppedMeta(t *testing.T, evt provider.ProviderEvent) provider.AgentWakeDroppedMeta {
	t.Helper()
	if evt.Kind != provider.EventAgentWakeDropped {
		t.Fatalf("kind = %s, want %s", evt.Kind, provider.EventAgentWakeDropped)
	}
	var meta provider.AgentWakeDroppedMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("meta: %v", err)
	}
	return meta
}

// The 2.1.284 capture: an agent parks on its own shell, its worktree is
// removed, and the shell reports. The CLI's only word on the failed wake
// is the mirrored queue-operation removal, after the shell's terminal and
// notification; nothing wakes the agent.
func TestReplayDroppedAgentWake(t *testing.T) {
	events := replayFixture(t, droppedWakeFixture)
	var dropped []provider.ProviderEvent
	shellNotified := -1
	for i, evt := range events {
		switch evt.Kind {
		case provider.EventAgentWakeDropped:
			dropped = append(dropped, evt)
			if shellNotified < 0 {
				t.Fatalf("dropped wake at event %d precedes the shell's notification", i)
			}
		case provider.EventBackgroundTaskNotification:
			if decodeEventMeta(t, evt)["task_id"] == "bn191xeyo" {
				shellNotified = i
			}
		case provider.EventUserText:
			if decodeEventMeta(t, evt)[provider.MetaSubagentWakePromptKey] == true {
				t.Fatalf("a wake prompt was parsed from a dropped wake: %+v", evt)
			}
		}
	}
	if len(dropped) != 1 {
		t.Fatalf("dropped wakes = %d, want 1", len(dropped))
	}
	meta := wakeDroppedMeta(t, dropped[0])
	want := provider.AgentWakeDroppedMeta{TaskID: "bn191xeyo", ToolUseID: "toolu_01KiGqDdgbxy144W69pHVtUK", Reason: "agent_stopped"}
	if meta != want {
		t.Fatalf("meta = %+v, want %+v", meta, want)
	}
}

// The same flow with the worktree intact: the wake is delivered, and the
// removal that records it is no drop.
func TestReplayMirroredDeliveredWakeIsNoDrop(t *testing.T) {
	events := replayFixture(t, mirrorWakeFixture)
	wakes := 0
	for _, evt := range events {
		if evt.Kind == provider.EventAgentWakeDropped {
			t.Fatalf("delivered wake reported as dropped: %+v", evt)
		}
		if evt.Kind == provider.EventUserText && decodeEventMeta(t, evt)[provider.MetaSubagentWakePromptKey] == true {
			wakes++
		}
	}
	if wakes != 1 {
		t.Fatalf("wakes = %d, want 1", wakes)
	}
}

func queueOperationLine(entries ...string) string {
	line := `{"type":"transcript_mirror","filePath":"/tmp/s.jsonl","entries":[`
	for i, entry := range entries {
		if i > 0 {
			line += ","
		}
		line += entry
	}
	return line + `]}`
}

func TestQueueOperationRemovalReasons(t *testing.T) {
	notification := `"<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>toolu_b1</tool-use-id>\n<status>completed</status>\n<summary>done</summary>\n</task-notification>"`
	cases := []struct {
		name  string
		entry string
		want  []provider.AgentWakeDroppedMeta
	}{
		{"agent stopped", `{"type":"queue-operation","operation":"remove","reason":"agent_stopped","content":` + notification + `}`,
			[]provider.AgentWakeDroppedMeta{{TaskID: "b1", ToolUseID: "toolu_b1", Reason: "agent_stopped"}}},
		{"resume failed", `{"type":"queue-operation","operation":"remove","reason":"resume_failed","content":` + notification + `}`,
			[]provider.AgentWakeDroppedMeta{{TaskID: "b1", ToolUseID: "toolu_b1", Reason: "resume_failed"}}},
		{"content blocks", `{"type":"queue-operation","operation":"remove","reason":"agent_stopped","content":[{"type":"text","text":` + notification + `}]}`,
			[]provider.AgentWakeDroppedMeta{{TaskID: "b1", ToolUseID: "toolu_b1", Reason: "agent_stopped"}}},
		{"monitor event names no tool use", `{"type":"queue-operation","operation":"remove","reason":"agent_stopped","content":"<task-notification>\n<task-id>w1</task-id>\n<summary>Monitor event</summary>\n<event>[Monitor expired]</event>\n</task-notification>"}`,
			[]provider.AgentWakeDroppedMeta{{TaskID: "w1", Reason: "agent_stopped"}}},
		{"delivered", `{"type":"queue-operation","operation":"remove","reason":"delivered_to_agent","content":` + notification + `}`, nil},
		{"absorbed", `{"type":"queue-operation","operation":"remove","reason":"absorbed_mid_turn","content":` + notification + `}`, nil},
		{"unknown reason", `{"type":"queue-operation","operation":"remove","reason":"something_new","content":` + notification + `}`, nil},
		{"enqueue", `{"type":"queue-operation","operation":"enqueue","content":` + notification + `}`, nil},
		{"not a notification", `{"type":"queue-operation","operation":"remove","reason":"agent_stopped","content":"hello"}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := parseOne(t, NewParser(), queueOperationLine(tc.entry))
			if len(events) != len(tc.want) {
				t.Fatalf("events = %+v, want %d drops", events, len(tc.want))
			}
			for i, evt := range events {
				if got := wakeDroppedMeta(t, evt); got != tc.want[i] {
					t.Fatalf("drop %d = %+v, want %+v", i, got, tc.want[i])
				}
			}
		})
	}
}

// Queue operations are session records: while a direct command waits to
// learn its transcript scope, they never enter the bounded buffer of
// transcript rows awaiting classification.
func TestQueueOperationsNeverEnterTheMirrorPendingBuffer(t *testing.T) {
	session := &Session{}
	session.directCommands.note("cmd-1", "/brainstorm", provider.SendOptions{})
	parser := NewParser()
	parser.peerTurns = session
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"started"}`)); err != nil {
		t.Fatalf("command start: %v", err)
	}
	events := parseOne(t, parser, queueOperationLine(
		`{"type":"queue-operation","operation":"enqueue","content":"x"}`,
		`{"type":"queue-operation","operation":"dequeue"}`,
	))
	if len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
	if state := parser.transcriptMirror; state != nil && (len(state.pending) != 0 || state.totalPendingBytes != 0) {
		t.Fatalf("queue operations buffered: %+v", state.pending)
	}
}

package claudetui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// taskNotificationXML builds the <task-notification> body Claude injects into a
// /v1/messages request when a backgrounded command/agent completes. Mirrors the
// upstream shape (LocalShellTask.tsx / LocalAgentTask.tsx): a statusless body is a
// stall progress ping, so callers pass status="" to exercise the skip path.
func taskNotificationXML(taskID, toolUseID, status, outputFile, summary string) string {
	statusLine := ""
	if status != "" {
		statusLine = fmt.Sprintf("\n<status>%s</status>", status)
	}
	return fmt.Sprintf(
		"<task-notification>\n<task-id>%s</task-id>\n<tool-use-id>%s</tool-use-id>\n<output-file>%s</output-file>%s\n<summary>%s</summary>\n</task-notification>",
		taskID, toolUseID, outputFile, statusLine, summary)
}

// bgResumeReqBody is a classAgent body whose latest user message is an injected
// <task-notification> — the shape of the resume request the CLI makes after a
// backgrounded task finished while the agent was between turns. The text rides a
// content block (array), matching the live wire.
func bgResumeReqBody(notificationXML string) string {
	return fmt.Sprintf(
		`{"model":"claude-haiku","max_tokens":32000,"tools":[{"name":"Bash"},{"name":"Read"}],`+
			`"messages":[{"role":"user","content":"run a background command"},`+
			`{"role":"assistant","content":[{"type":"text","text":"starting it"}]},`+
			`{"role":"user","content":[{"type":"text","text":%q}]}]}`, notificationXML)
}

// bgResumeReqBodyMulti builds a classAgent body whose successive user messages
// each carry one of the given <task-notification>s, for exercising the
// discriminator's multi-notification scan (the self-match need not be first).
func bgResumeReqBodyMulti(notifs ...string) string {
	msgs := make([]string, 0, len(notifs))
	for _, n := range notifs {
		msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":%q}]}`, n))
	}
	return fmt.Sprintf(
		`{"model":"claude-haiku","max_tokens":32000,"tools":[{"name":"Bash"}],"messages":[%s]}`,
		strings.Join(msgs, ","))
}

// metaField pulls a string field out of a ProviderEvent's Meta JSON.
func metaField(t *testing.T, ev provider.ProviderEvent, key string) string {
	t.Helper()
	if len(ev.Meta) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(ev.Meta, &m); err != nil {
		t.Fatalf("unmarshal meta %s: %v", ev.Meta, err)
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// idxOfKind returns the index of the first event of kind, or -1.
func idxOfKind(events []provider.ProviderEvent, kind provider.EventKind) int {
	for i, ev := range events {
		if ev.Kind == kind {
			return i
		}
	}
	return -1
}

// TestReconstructBackgroundCompletionFromRequestBody pins the core fix: a
// backgrounded task's completion crosses the /v1/messages wire ONLY as a
// <task-notification> in the request body (the stream-json system/task_updated +
// system/task_notification headless emits are CLI-internal). claude-tui must
// reconstruct BOTH from that body — task_updated (→ EventBackgroundTaskTerminal,
// which triage stashes) then task_notification (→ EventBackgroundTaskNotification,
// which drains the stash and writes the tool_completion sibling). Without
// emitBackgroundCompletions the body is dropped and neither event fires, so the
// launch stays "running" forever — exactly the reported bug.
//
// The triage half (stash → sibling) is byte-identical to the headless path and is
// covered by triage's own tests; here we assert the provider produces the right
// envelopes in the right order.
func TestReconstructBackgroundCompletionFromRequestBody(t *testing.T) {
	rp := newReconParser(t)

	notif := taskNotificationXML("bgtask1", "toolu_bg", "completed", "/tmp/out.txt",
		`Background command "tick" completed (exit code 0)`)
	rp.drive("", bgResumeReqBody(notif), endTurnSSE())

	terminals := findKind(rp.out, provider.EventBackgroundTaskTerminal)
	if len(terminals) != 1 {
		t.Fatalf("EventBackgroundTaskTerminal=%d want 1 (kinds %v)", len(terminals), kindsOf(rp.out))
	}
	notifs := findKind(rp.out, provider.EventBackgroundTaskNotification)
	if len(notifs) != 1 {
		t.Fatalf("EventBackgroundTaskNotification=%d want 1 (kinds %v)", len(notifs), kindsOf(rp.out))
	}

	// task_updated must precede task_notification: triage stashes the terminal,
	// then the notification drains that stash to write the sibling. Reversed, the
	// drain finds nothing and the launch never completes.
	if ti, ni := idxOfKind(rp.out, provider.EventBackgroundTaskTerminal),
		idxOfKind(rp.out, provider.EventBackgroundTaskNotification); ti > ni {
		t.Fatalf("task_updated (idx %d) must be emitted before task_notification (idx %d): %v", ti, ni, kindsOf(rp.out))
	}

	if got := metaField(t, terminals[0], "task_id"); got != "bgtask1" {
		t.Fatalf("terminal task_id=%q want bgtask1", got)
	}
	if got := metaField(t, terminals[0], "status"); got != "completed" {
		t.Fatalf("terminal status=%q want completed", got)
	}
	if got := metaField(t, terminals[0], "source"); got != "task_updated" {
		t.Fatalf("terminal source=%q want task_updated", got)
	}
	if got := terminals[0].ItemID; got != "toolu_bg" {
		t.Fatalf("terminal tool_use_id=%q want toolu_bg — the launch the sibling completes", got)
	}
	if got := metaField(t, notifs[0], "output_file"); got != "/tmp/out.txt" {
		t.Fatalf("notification output_file=%q want /tmp/out.txt — the command output triage reads", got)
	}
	if got := notifs[0].ItemID; got != "toolu_bg" {
		t.Fatalf("notification tool_use_id=%q want toolu_bg", got)
	}
}

// TestReconstructBackgroundStallPingSkipped pins the user requirement that an
// inline-style "still running" signal must NOT show a completion: a statusless
// <task-notification> is a stall progress ping (the command is blocked on input,
// not done), so it reconstructs no terminal and no notification. print.ts treats
// the absence of <status> as non-terminal; we mirror that.
func TestReconstructBackgroundStallPingSkipped(t *testing.T) {
	rp := newReconParser(t)

	ping := taskNotificationXML("bgtask1", "toolu_bg", "", "/tmp/out.txt",
		`Background command "tick" appears to be waiting for interactive input`)
	rp.drive("", bgResumeReqBody(ping), endTurnSSE())

	if got := len(findKind(rp.out, provider.EventBackgroundTaskTerminal)); got != 0 {
		t.Fatalf("EventBackgroundTaskTerminal=%d want 0 — a statusless stall ping is not a terminal (kinds %v)", got, kindsOf(rp.out))
	}
	if got := len(findKind(rp.out, provider.EventBackgroundTaskNotification)); got != 0 {
		t.Fatalf("EventBackgroundTaskNotification=%d want 0 — a stall ping must not synthesize completion events", got)
	}
}

// TestReconstructBackgroundCompletionDedup pins the seen-set: the terminal
// <task-notification> stays in conversation history and recurs in every later
// request body, but it must reconstruct the completion exactly once.
func TestReconstructBackgroundCompletionDedup(t *testing.T) {
	rp := newReconParser(t)

	notif := taskNotificationXML("bgtask1", "toolu_bg", "completed", "/tmp/out.txt", "done")
	// First resume reconstructs the completion.
	rp.drive("", bgResumeReqBody(notif), endTurnSSE())
	// A later request still carries the notification in history — must not re-fire.
	rp.drive("", bgResumeReqBody(notif), endTurnSSE())

	if got := len(findKind(rp.out, provider.EventBackgroundTaskTerminal)); got != 1 {
		t.Fatalf("EventBackgroundTaskTerminal=%d want 1 — a recurring notification reconstructs once (kinds %v)", got, kindsOf(rp.out))
	}
	if got := len(findKind(rp.out, provider.EventBackgroundTaskNotification)); got != 1 {
		t.Fatalf("EventBackgroundTaskNotification=%d want 1 — dedup by task_id", got)
	}
}

// TestReconstructBackgroundCompletionUnroutableSkipped pins the ok=false branch
// of the shared eachTaskNotification scan: a <task-notification> with no usable
// task-id has nothing to key the completion sibling on, so it reconstructs
// nothing rather than synthesizing a malformed completion.
func TestReconstructBackgroundCompletionUnroutableSkipped(t *testing.T) {
	rp := newReconParser(t)

	noID := "<task-notification>\n<tool-use-id>toolu_bg</tool-use-id>\n<status>completed</status>\n</task-notification>"
	rp.drive("", bgResumeReqBody(noID), endTurnSSE())

	if got := len(findKind(rp.out, provider.EventBackgroundTaskTerminal)); got != 0 {
		t.Fatalf("EventBackgroundTaskTerminal=%d want 0 — a task-notification with no task-id is unroutable (kinds %v)", got, kindsOf(rp.out))
	}
	if got := len(findKind(rp.out, provider.EventBackgroundTaskNotification)); got != 0 {
		t.Fatalf("EventBackgroundTaskNotification=%d want 0 — an unroutable notification must not synthesize completion events", got)
	}
}

// TestRequestReportsAgentCompletion pins the gateway's main-vs-subagent
// discriminator for a header-bearing request: it is the MAIN loop observing a
// backgrounded subagent's completion iff the body carries a <task-notification>
// whose task-id equals the agent-id (a backgrounded subagent's task_id IS its
// agent_id). A genuine subagent turn — even one polling a backgrounded CHILD —
// never reports its own completion, so it must read false. Status is not checked:
// a still-running self-ping is equally the main loop, not the subagent's turn.
func TestRequestReportsAgentCompletion(t *testing.T) {
	selfDone := taskNotificationXML("aid-self", "toolu_launch", "completed", "/tmp/o", "done")
	selfPing := taskNotificationXML("aid-self", "toolu_launch", "", "", "still running")
	childDone := taskNotificationXML("child-task", "toolu_child", "completed", "/tmp/o", "done")
	// A <task-notification> with no <task-id> is unroutable (ok=false), so it can
	// never be a self-match regardless of the agent-id.
	noTaskID := "<task-notification>\n<tool-use-id>toolu_x</tool-use-id>\n<status>completed</status>\n</task-notification>"

	cases := []struct {
		name    string
		body    string
		agentID string
		want    bool
	}{
		{"self-reporting completion is the main loop", bgResumeReqBody(selfDone), "aid-self", true},
		{"statusless self-ping is still the main loop", bgResumeReqBody(selfPing), "aid-self", true},
		{"child task-notification stays a subagent turn", bgResumeReqBody(childDone), "aid-self", false},
		{"no task-notification stays a subagent turn", agentReqBody, "aid-self", false},
		{"missing task-id is unroutable, never a match", bgResumeReqBody(noTaskID), "aid-self", false},
		{"self-match found when it is not the first notification", bgResumeReqBodyMulti(childDone, selfDone), "aid-self", true},
		{"empty agent-id is never an observation", bgResumeReqBody(selfDone), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, req := classifyRequest([]byte(tc.body))
			if req == nil {
				t.Fatalf("classifyRequest(%s) returned nil req", tc.body)
			}
			if got := requestReportsAgentCompletion(req.Messages, tc.agentID); got != tc.want {
				t.Fatalf("requestReportsAgentCompletion = %v, want %v", got, tc.want)
			}
		})
	}
}

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

// systemNotificationEnvelope wraps a <task-notification> in the
// "[SYSTEM NOTIFICATION - NOT USER INPUT]" preamble the CLI emits when it
// flushes a backgrounded completion into a request body (the live wire shape
// captured by the CLI capture spike taskoutput_siblings).
func systemNotificationEnvelope(notif string) string {
	return "[SYSTEM NOTIFICATION - NOT USER INPUT]\n" +
		"This is an automated background-task event, NOT a message from the user.\n" +
		"Do NOT interpret this as user acknowledgement, confirmation, or response to any pending question.\n\n" +
		notif
}

// bgSystemBundleReqBody is the real-wire shape captured when sibling background
// commands finish during a TaskOutput(block=true) wait: ONE role:"system"
// message whose STRING content bundles every completed task's
// "[SYSTEM NOTIFICATION ...]" + <task-notification>, blank-line separated. This
// is NOT the role:"user" array-content shape bgResumeReqBody models — it is the
// shape the old role=="user", first-only code silently dropped.
func bgSystemBundleReqBody(notifs ...string) string {
	envelopes := make([]string, 0, len(notifs))
	for _, n := range notifs {
		envelopes = append(envelopes, systemNotificationEnvelope(n))
	}
	// %q (NOT json.Marshal) keeps the angle brackets LITERAL. The real CLI is
	// Node, whose JSON.stringify does not HTML-escape '<', so the wire carries a
	// literal "<task-notification" that the byte-probe needle matches. Go's
	// json.Marshal would emit < and the probe would miss it — a Go-encoder
	// artifact, not the wire (the aocap capture shows the same < for the
	// same reason).
	return fmt.Sprintf(
		`{"model":"claude-haiku","max_tokens":32000,"tools":[{"name":"Bash"}],`+
			`"messages":[{"role":"user","content":"run two background commands"},`+
			`{"role":"assistant","content":[{"type":"text","text":"started both"}]},`+
			`{"role":"system","content":%q}]}`, strings.Join(envelopes, "\n\n"))
}

// assistantQuotedNotificationReqBody is a classAgent body where the MODEL
// quoted a <task-notification> verbatim in its own assistant output. The tag
// rides a role:"assistant" message (not a CLI-injected user/system one), so the
// byte-probe matches but the role gate must reject it — otherwise the model
// could fabricate completion rows by echoing the tag. A trailing user message
// keeps the body a normal-shaped agent turn.
func assistantQuotedNotificationReqBody(notif string) string {
	return fmt.Sprintf(
		`{"model":"claude-haiku","max_tokens":32000,"tools":[{"name":"Bash"},{"name":"Read"}],`+
			`"messages":[{"role":"user","content":"run a background command"},`+
			`{"role":"assistant","content":[{"type":"text","text":%q}]},`+
			`{"role":"user","content":"thanks"}]}`, notif)
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

// TestReconstructBackgroundCompletionFromSystemBundle is the regression test
// for the reported bug. When a sibling backgrounded command finishes while the
// agent is blocked on TaskOutput(block=true), the CLI flushes the completions
// as a SINGLE role:"system" message bundling a <task-notification> per
// completed task — the TaskOutput-waited one AND the sibling (the exact shape
// the CLI capture spike taskoutput_siblings captured on 2.1.170). The old
// code dropped it twice over: it skipped any message whose role != "user", and
// it extracted only the first of several notifications. So both launches stayed
// "running" forever. After the fix both reconstruct the task_updated +
// task_notification pair, each keyed to its OWN launch via its explicit
// <tool-use-id> so the sibling completion lands on the right row.
func TestReconstructBackgroundCompletionFromSystemBundle(t *testing.T) {
	rp := newReconParser(t)

	n5 := taskNotificationXML("bg5s", "toolu_5s", "completed", "/tmp/5s.out",
		`Background command "5s ticks" completed (exit code 0)`)
	n10 := taskNotificationXML("bg10s", "toolu_10s", "completed", "/tmp/10s.out",
		`Background command "10s ticks" completed (exit code 0)`)
	rp.drive("", bgSystemBundleReqBody(n5, n10), endTurnSSE())

	terminals := findKind(rp.out, provider.EventBackgroundTaskTerminal)
	if len(terminals) != 2 {
		t.Fatalf("EventBackgroundTaskTerminal=%d want 2 — both bundled tasks (kinds %v)", len(terminals), kindsOf(rp.out))
	}
	notifs := findKind(rp.out, provider.EventBackgroundTaskNotification)
	if len(notifs) != 2 {
		t.Fatalf("EventBackgroundTaskNotification=%d want 2 (kinds %v)", len(notifs), kindsOf(rp.out))
	}

	// Each task resolves to its own launch via the explicit <tool-use-id>, so a
	// sibling completion can't cross-wire onto the wrong launch row.
	termByTask := map[string]string{} // task_id -> tool_use_id (ItemID)
	for _, tv := range terminals {
		termByTask[metaField(t, tv, "task_id")] = tv.ItemID
		if got := metaField(t, tv, "source"); got != "task_updated" {
			t.Fatalf("terminal source=%q want task_updated", got)
		}
	}
	if termByTask["bg5s"] != "toolu_5s" {
		t.Fatalf("5s terminal tool_use_id=%q want toolu_5s (mapping %v)", termByTask["bg5s"], termByTask)
	}
	if termByTask["bg10s"] != "toolu_10s" {
		t.Fatalf("10s terminal tool_use_id=%q want toolu_10s (mapping %v)", termByTask["bg10s"], termByTask)
	}
}

// TestReconstructBackgroundCompletionMixedBundle pins the continue-not-stop
// contract eachTaskNotification's loop relies on: one skipped or malformed
// block in a coalesced bundle must never strand the others. Here an unroutable
// (no <task-id>) block and a statusless stall ping sit BETWEEN two real
// terminals, and both terminals still reconstruct, each keyed to its own
// launch. A regression — an early return, or a malformed block truncating the
// scan — would resurrect the stuck-running bug for the trailing task.
func TestReconstructBackgroundCompletionMixedBundle(t *testing.T) {
	rp := newReconParser(t)

	n5 := taskNotificationXML("bg5s", "toolu_5s", "completed", "/tmp/5s.out", "5s done")
	// No <task-id> → unroutable; must be skipped without stopping the loop.
	noID := "<task-notification>\n<tool-use-id>toolu_orphan</tool-use-id>\n<status>completed</status>\n</task-notification>"
	// No <status> → statusless stall ping; skipped, but the loop continues.
	ping := taskNotificationXML("bgstall", "toolu_stall", "", "", "waiting for input")
	n10 := taskNotificationXML("bg10s", "toolu_10s", "completed", "/tmp/10s.out", "10s done")
	rp.drive("", bgSystemBundleReqBody(n5, noID, ping, n10), endTurnSSE())

	terminals := findKind(rp.out, provider.EventBackgroundTaskTerminal)
	termByTask := map[string]string{} // task_id -> tool_use_id (ItemID)
	for _, tv := range terminals {
		termByTask[metaField(t, tv, "task_id")] = tv.ItemID
	}
	if len(terminals) != 2 || termByTask["bg5s"] != "toolu_5s" || termByTask["bg10s"] != "toolu_10s" {
		t.Fatalf("want exactly the two terminal tasks {bg5s:toolu_5s, bg10s:toolu_10s}, got %v (kinds %v)", termByTask, kindsOf(rp.out))
	}
	if _, leaked := termByTask["bgstall"]; leaked {
		t.Fatalf("statusless stall ping leaked a terminal: %v", termByTask)
	}
}

// TestReconstructBackgroundCompletionAssistantRoleIgnored pins the role
// discriminator: a <task-notification> the MODEL quoted in its own assistant
// output is not a completion signal and must reconstruct nothing. Only the
// CLI's injected user/system messages carry genuine completions; accepting the
// assistant role would let the model fabricate completion rows by echoing the
// tag.
func TestReconstructBackgroundCompletionAssistantRoleIgnored(t *testing.T) {
	rp := newReconParser(t)

	notif := taskNotificationXML("bgquoted", "toolu_q", "completed", "/tmp/o", "quoted by the model")
	rp.drive("", assistantQuotedNotificationReqBody(notif), endTurnSSE())

	if got := len(findKind(rp.out, provider.EventBackgroundTaskTerminal)); got != 0 {
		t.Fatalf("EventBackgroundTaskTerminal=%d want 0 — a tag quoted in assistant output is not a completion (kinds %v)", got, kindsOf(rp.out))
	}
	if got := len(findKind(rp.out, provider.EventBackgroundTaskNotification)); got != 0 {
		t.Fatalf("EventBackgroundTaskNotification=%d want 0 — assistant-quoted tag must not synthesize completion events", got)
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

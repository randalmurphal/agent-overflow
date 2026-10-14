package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// agentMessageMetas is every attribution a user row can carry, with whether
// it is an agent's message (usermessage.IsAgentMessage).
var agentMessageMetas = []struct {
	name  string
	meta  string
	agent bool
}{
	{"the person's", ``, false},
	{"the person's with a send id", `{"sendId":"s1"}`, false},
	{"outside queue", `{"origin":"external-queue"}`, false},
	{"scheduled", `{"origin":"scheduled"}`, false},
	{"agent thread request", `{"origin":"agent-thread","originThread":{"threadId":"t-x","kind":"send"},"sendId":"thread-request:k"}`, true},
	{"agent thread wake", `{"origin":"agent-thread","originThread":{"threadId":"t-x","kind":"wake"},"sendId":"thread-wake:k"}`, true},
	{"loop wake", `{"origin":"agent-wake","originWake":{"kind":"loop","label":"l"}}`, true},
	{"remote job completion", `{"origin":"agent-wake","originWake":{"kind":"completion","label":"build","computer":"box"}}`, true},
	{"workflow wake", `{"origin":"agent-wake","originWake":{"kind":"workflow","label":"wf-1"}}`, true},
	{"the person's joined after a wake", `{"origin":"agent-wake","originWake":{"kind":"loop"},"expandComposerCommands":true}`, false},
}

// The classifier exists three times: usermessage.IsAgentMessage, the store's
// AgentMessageFilterFor behind the nav rail's ticks, and the frontend's
// isAgentMessageMeta (pinned by its own tests against the same cases). This
// holds the first two to one answer per row.
func TestAgentMessageClassifierAgreesInGoAndSQL(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "classify")
	items := make([]store.Item, 0, len(agentMessageMetas))
	for i, tc := range agentMessageMetas {
		item := textItem("row-"+string(rune('a'+i)), "user_text", tc.name)
		item.Meta = tc.meta
		items = append(items, item)
	}
	f.turn(t, thread.ID, 0, 1_000, items...)

	ticks, err := f.app.store.ListThreadUserMessageTicks(thread.ID, store.TimelineSelection{})
	if err != nil {
		t.Fatalf("ListThreadUserMessageTicks: %v", err)
	}
	ticked := map[string]bool{}
	for _, tick := range ticks {
		ticked[tick.ID] = true
	}
	history, err := f.app.store.ListThreadUserMessageHistory(thread.ID, 50)
	if err != nil {
		t.Fatalf("ListThreadUserMessageHistory: %v", err)
	}
	recalled := map[string]bool{}
	for _, entry := range history {
		recalled[entry.ID] = true
	}
	for i, tc := range agentMessageMetas {
		id := items[i].ID
		meta, decodeErr := usermessage.FromItem(store.Item{Meta: tc.meta})
		goAgent := decodeErr == nil && usermessage.IsAgentMessage(meta)
		if goAgent != tc.agent {
			t.Errorf("%s: IsAgentMessage = %v, want %v", tc.name, goAgent, tc.agent)
		}
		if ticked[id] == tc.agent || recalled[id] == tc.agent {
			t.Errorf("%s: ticked %v, recalled %v; an agent's message is neither and the person's is both", tc.name, ticked[id], recalled[id])
		}
		// The send-identity read still finds an agent's row by its send id:
		// that is how a retried request or wake is recognised.
		var ids struct {
			SendID string `json:"sendId"`
		}
		if json.Unmarshal([]byte(tc.meta), &ids) == nil && ids.SendID != "" {
			if found, ok, err := f.app.store.FindUserTextItemBySendID(thread.ID, ids.SendID); err != nil || !ok || found.ID != id {
				t.Errorf("%s: FindUserTextItemBySendID = %s %v %v", tc.name, found.ID, ok, err)
			}
		}
	}
}

// thread_show and the export name an agent's message as an agent's, with
// its sender, never as the user.
func TestThreadShowLabelsAgentMessagesWithTheirSender(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "show-agents")
	person := textItem("person", "user_text", "my own words")
	request := textItem("request", "user_text", "do it")
	request.Meta = `{"origin":"agent-thread","originThread":{"threadId":"t-lead","title":"Lead","computerName":"desk","kind":"send"}}`
	loop := textItem("loop", "user_text", "Loop fired")
	loop.Meta = `{"origin":"agent-wake","originWake":{"kind":"loop","label":"nightly"}}`
	worker := textItem("worker", "user_text", "Thread asks")
	worker.Meta = `{"origin":"agent-wake","originWake":{"kind":"approval","label":"Builder","threadId":"t-w"}}`
	script := textItem("script", "user_text", "from cron")
	script.Meta = `{"origin":"agent-wake","originWake":{"kind":"send"}}`
	peer := textItem("peer", "user_text", "hi from beta")
	peer.Meta = `{"origin":"peer-session","wire_only":true,"cross_session_from_name":"BETA"}`
	job := textItem("job", "user_text", "Remote job finished")
	job.Meta = `{"origin":"agent-wake","originWake":{"kind":"completion","label":"nightly build","computer":"GPU box"}}`
	flow := textItem("flow", "user_text", "Workflow wake")
	flow.Meta = `{"origin":"agent-wake","originWake":{"kind":"workflow","label":"release"}}`
	mixed := textItem("mixed", "user_text", "wake then mine")
	mixed.Meta = `{"origin":"agent-wake","originWake":{"kind":"loop"},"expandComposerCommands":true}`
	f.turn(t, thread.ID, 0, 1_000, person, request, loop, worker, script, peer, job, flow, mixed)

	bounds, err := f.adapter.ResolveWindow(t.Context(), threadtools.WindowQuery{ThreadID: thread.ID, Kind: threadtools.WindowAll})
	if err != nil {
		t.Fatalf("ResolveWindow: %v", err)
	}
	page, err := f.adapter.Transcript(t.Context(), threadtools.TranscriptQuery{ThreadID: thread.ID, From: bounds.From, To: bounds.To, Limit: 20})
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	want := map[string]string{
		"person":  "[user person]",
		"request": `[agent request from "Lead" (t-lead, on desk)]`,
		"loop":    `[agent loop from loop "nightly"]`,
		"worker":  `[agent worker from "Builder" (t-w), its approval]`,
		"script":  `[agent script from a script (agent-overflow thread send)]`,
		"peer":    `[agent peer from Claude session "BETA"]`,
		"job":     `[agent job from remote job "nightly build" on GPU box]`,
		"flow":    `[agent flow from workflow "release"]`,
		"mixed":   "[user mixed]",
	}
	if len(page.Items) != len(want) {
		t.Fatalf("page = %+v", page.Items)
	}
	for _, item := range page.Items {
		if got := threadtools.RowHead(item); got != want[item.ID] {
			t.Errorf("%s: head %q, want %q", item.ID, got, want[item.ID])
		}
	}
}

// thread_search's last_message says an agent spoke last, not the user: an
// idle thread whose last word was another agent's request is not waiting on
// its person.
func TestThreadSearchLastMessageNamesAnAgent(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	agentLast := f.thread(t, "agent-last")
	request := textItem("req", "user_text", "please review")
	request.Meta = `{"origin":"agent-thread","originThread":{"threadId":"t-lead"}}`
	f.turn(t, agentLast.ID, 0, 1_000, request)
	personLast := f.thread(t, "person-last")
	f.turn(t, personLast.ID, 0, 1_000, textItem("ask", "user_text", "are you there?"))

	hits := []threadtools.Hit{{Thread: threadtools.Thread{ID: agentLast.ID}}, {Thread: threadtools.Thread{ID: personLast.ID}}}
	if err := (threadToolsApp{app: f.app}).attachLastMessages(hits); err != nil {
		t.Fatalf("attachLastMessages: %v", err)
	}
	if hits[0].LastMessage == nil || hits[0].LastMessage.Role != "agent" {
		t.Errorf("agent's request = %+v, want role agent", hits[0].LastMessage)
	}
	if hits[1].LastMessage == nil || hits[1].LastMessage.Role != "user" {
		t.Errorf("person's ask = %+v, want role user", hits[1].LastMessage)
	}
}

// A spawn with no title is named from the prompt, not from the footer the
// prompt carries: a title read off the routing would be "Agent request from
// thread ...".
func TestUntitledSpawnIsNamedFromItsPromptNotItsFooter(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "done")
	named := make(chan string, 1)
	f.app.generateThreadTitleFn = func(_ store.Thread, message string, _ []store.Attachment) (string, error) {
		named <- message
		return "Windows build", nil
	}

	if _, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "check the windows build",
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case message := <-named:
		if message != "check the windows build" {
			t.Errorf("title generated from %q, want the prompt alone", message)
		}
		if strings.Contains(message, "Agent request") {
			t.Errorf("title generation read the footer: %q", message)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no title was generated for an untitled spawn")
	}
}

// The flush acknowledgement tells the composer's preview whether the row a
// dispatch wrote is an agent's: a drain of wakes alone is, and the same drain
// joined with the person's composer message is the person's, as the joined
// row itself is (usermessage.IsAgentMessage). The live-state snapshot that
// replaces those acknowledgements after a reconnect says the same.
func TestFlushAcknowledgementClassifiesTheJoinedRow(t *testing.T) {
	t.Parallel()
	wake := flushQueuePayload{
		SendID: "agent-wake:w1", Origin: usermessage.OriginAgentWake,
		OriginWake: &usermessage.OriginWake{Kind: "loop", Label: "nightly"},
	}
	person := flushQueuePayload{SendID: "send-person", ExpandComposerCommands: true}
	for _, tc := range []struct {
		name    string
		members []flushQueuePayload
		agent   bool
	}{
		{"wakes alone", []flushQueuePayload{wake, {SendID: "agent-wake:w2", Origin: usermessage.OriginAgentWake, OriginWake: wake.OriginWake}}, true},
		{"a wake joined with the person's message", []flushQueuePayload{wake, person}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, rec := newAppForFlushQueueRPC(t)
			workspace := initGitRepo(t)
			thread := testThread("flush-agent-" + strings.ReplaceAll(tc.name, " ", "-"))
			thread.Provider = string(provider.Claude)
			thread.WorkspacePath = workspace
			thread.SessionRef = "agent-flush-session"
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatalf("CreateThread: %v", err)
			}
			now := time.Now().UnixMilli()
			if err := app.store.InsertTurn(store.Turn{TurnID: "turn-1", ThreadID: thread.ID, TurnIndex: 1, StartedAt: now}); err != nil {
				t.Fatalf("InsertTurn: %v", err)
			}
			if _, err := app.store.AppendItem(store.Item{
				ID: "toolu_wait", ThreadID: thread.ID, TurnIndex: 1, Kind: "tool_call", Role: "assistant",
				Status: "running", Summary: "sleep 10", CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("seed tool_call: %v", err)
			}
			sess, err := claude.NewSession(context.Background(), thread.ID,
				claude.Config{Binary: writeClaudeStdinRecorderBinary(t, filepath.Join(t.TempDir(), "stdin.jsonl")), WorkDir: workspace},
				func(provider.ProviderEvent) {})
			if err != nil {
				t.Fatalf("claude.NewSession: %v", err)
			}
			t.Cleanup(func() { _ = sess.Close() })
			app.sessionManager().put(thread.ID, session{
				Provider: string(provider.Claude), Token: "tok", Claude: sess, Liveness: newSessionLiveness(time.Now()),
			})
			items := make([]triage.QueuedFlushItem, 0, len(tc.members))
			for i, payload := range tc.members {
				items = append(items, triage.QueuedFlushItem{
					ID: fmt.Sprintf("queue:%d", i), Message: fmt.Sprintf("member %d", i),
					Payload: flushQueuePayloadJSON(t, payload), EnqueuedAt: now + int64(1+i),
				})
			}
			app.dispatchFlush(thread.ID, items)

			var acks []QueueFlushedItem
			for _, call := range rec.snapshot() {
				if evt, ok := call.Data.(QueueFlushedEvent); ok && call.Channel == "provider:queue_flushed" {
					acks = append(acks, evt.Items...)
				}
			}
			if len(acks) != len(tc.members) {
				t.Fatalf("acknowledgements = %+v", acks)
			}
			for _, ack := range acks {
				if ack.AgentMessage != tc.agent {
					t.Errorf("acknowledgement %s: AgentMessage = %v, want %v", ack.QueueItemID, ack.AgentMessage, tc.agent)
				}
			}
			live, err := app.GetThreadLiveState(thread.ID)
			if err != nil {
				t.Fatalf("GetThreadLiveState: %v", err)
			}
			if len(live.FlushedItems) == 0 {
				t.Fatalf("live state holds no flushed message: %+v", live)
			}
			for _, flushed := range live.FlushedItems {
				if flushed.AgentMessage != tc.agent {
					t.Errorf("live flushed %s: AgentMessage = %v, want %v", flushed.QueueItemID, flushed.AgentMessage, tc.agent)
				}
			}
		})
	}
}

// A finished remote job's report is the app's message, not the person's:
// it queues attributed to the job and its computer, and both the queue push
// and the live-state snapshot mark it as an agent's message, so the
// composer's preview leaves it out.
func TestRemoteCompletionQueuesAsAnAgentMessage(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return nil }
	thread := remoteWatchThread(t, a, string(provider.Claude))
	w := completedRemoteWatch(t, a, thread)
	if err := a.deliverRemoteCompletion(w, remoteCompletionOutput{Tail: w.Receipt.Output}); err != nil {
		t.Fatal(err)
	}
	want := usermessage.OriginWake{Kind: usermessage.OriginWakeKindCompletion, Label: w.Label, Computer: w.ComputerID}
	check := func(source string, item QueuedItem) {
		t.Helper()
		if !item.AgentMessage || item.Origin != usermessage.OriginAgentWake || item.OriginWake == nil || *item.OriginWake != want {
			t.Fatalf("%s: queued completion = agent %v origin %q wake %+v, want an agent message from %+v", source, item.AgentMessage, item.Origin, item.OriginWake, want)
		}
	}
	state := rec.lastQueueState(t)
	if len(state.Items) != 1 {
		t.Fatalf("queue push: %+v", state)
	}
	check("queue push", state.Items[0])
	live, err := a.GetThreadLiveState(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.QueueItems) != 1 {
		t.Fatalf("live state queue: %+v", live.QueueItems)
	}
	check("live state", live.QueueItems[0])
}

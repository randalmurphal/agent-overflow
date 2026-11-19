package app

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

const requeuedRequestText = "Fix the nil check.\n\n---\nAgent request from thread \"Lead\" (t-lead).\nIt is not waiting."

const requeuedRequestMeta = `{"origin":"agent-thread","originThread":{"threadId":"t-lead","title":"Lead","kind":"send","token":"tok-1"},"sendId":"thread-request:tok-1"}`

func requireAgentPayload(t *testing.T, name string, raw json.RawMessage) {
	t.Helper()
	var payload flushQueuePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("%s: decode payload: %v", name, err)
	}
	if payload.Origin != agentmeta.OriginAgentThread || payload.OriginThread == nil || payload.OriginThread.Token != "tok-1" {
		t.Fatalf("%s: requeued payload lost who wrote it: %s", name, raw)
	}
}

// A message requeued after a failed restore, a failed stale-row cleanup or a
// failed Codex resend keeps who wrote it, so its next row is the agent's and
// never the person's to take back into the composer.
func TestARequeuedMessageKeepsWhoWroteIt(t *testing.T) {
	t.Parallel()
	row := store.Item{ID: "u:0", ThreadID: "t", Kind: "user_text", Summary: requeuedRequestText, Meta: requeuedRequestMeta}
	for name, item := range map[string]triage.UnconfirmedFlushItem{
		"deferred": {QueueItemID: "q1", Message: requeuedRequestText, DeferredItem: &row},
		"quiet":    {QueueItemID: "q1", Message: requeuedRequestText, QuietItem: &row},
	} {
		queued, ok := queuedFlushItemFromUnconfirmed(item)
		if !ok {
			t.Fatalf("%s: not requeued", name)
		}
		requireAgentPayload(t, name, queued.Payload)
	}
	raw, err := flushPayloadFromUserMeta(requeuedRequestMeta)
	if err != nil {
		t.Fatal(err)
	}
	requireAgentPayload(t, "eager", raw)
}

// A requeued agent's message goes back as it was written, so the facts it
// carries still describe its text and its row reads the same body; the
// person's text goes back without the white space around it.
func TestARequeuedAgentMessageKeepsTheTextItsFactsDescribe(t *testing.T) {
	t.Parallel()
	spawn, err := usermessage.Request(store.ThreadRequestSpawn, usermessage.Sender{ThreadID: "t-lead", Title: "Lead"},
		threadtools.Footer{Title: "Lead", ThreadID: "t-lead", Token: "tok-1", AnswerRequested: true}, "\n\n  Build   the docs  \nand more")
	if err != nil {
		t.Fatal(err)
	}
	attribution := spawn.Attribution()
	meta, err := usermessage.MarshalMeta(usermessage.Meta{Origin: attribution.Origin, OriginThread: attribution.Thread, SendID: "thread-request:tok-1"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := flushPayloadFromUserMeta(meta)
	if err != nil {
		t.Fatal(err)
	}
	row := store.Item{ID: "u:0", ThreadID: "t", Kind: "user_text", Summary: spawn.Text(), Meta: meta}
	for name, item := range map[string]triage.UnconfirmedFlushItem{
		"queued":   {QueueItemID: "q1", Message: spawn.Text(), Payload: payload},
		"deferred": {QueueItemID: "q1", Message: spawn.Text(), DeferredItem: &row},
		"quiet":    {QueueItemID: "q1", Message: spawn.Text(), QuietItem: &row},
	} {
		queued, ok := queuedFlushItemFromUnconfirmed(item)
		if !ok {
			t.Fatalf("%s: not requeued", name)
		}
		var requeued flushQueuePayload
		if err := json.Unmarshal(queued.Payload, &requeued); err != nil {
			t.Fatalf("%s: decode payload: %v", name, err)
		}
		read := agentmeta.Describe(queued.Message, agentmeta.Meta{Origin: requeued.Origin, OriginThread: requeued.OriginThread})
		if queued.Message != spawn.Text() || len(read) != 1 || read[0].Facts.Form != threadtools.FormRequest || read[0].Body() != "Build   the docs  \nand more" {
			t.Fatalf("%s: requeued %q reads as %+v", name, queued.Message, read)
		}
	}
	mine := store.Item{ID: "u:1", ThreadID: "t", Kind: "user_text", Summary: "  my words \n", Meta: `{"sendId":"s"}`}
	for name, item := range map[string]triage.UnconfirmedFlushItem{
		"queued":   {QueueItemID: "q2", Message: "  my words \n"},
		"deferred": {QueueItemID: "q2", Message: "  my words \n", DeferredItem: &mine},
	} {
		if queued, ok := queuedFlushItemFromUnconfirmed(item); !ok || queued.Message != "my words" {
			t.Fatalf("%s: the person's text requeued as %q", name, queued.Message)
		}
	}
}

// A pre-init session that dies with an agent's request handed to it and the
// stale row's cleanup failing requeues the request; the queue still shows it
// as the agent's message, and its dispatch writes the agent's row
// (TestFlushAcknowledgementClassifiesTheJoinedRow).
func TestAPreInitDeathRequeuesAnAgentsMessageAsTheAgents(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("preinit-agent-requeue")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	row := store.Item{
		ID: "user:0:flush:1", ThreadID: thread.ID, TurnIndex: 0, Kind: "user_text", Role: "user",
		Status: "completed", Summary: requeuedRequestText, Meta: requeuedRequestMeta,
	}
	app.triage.RegisterPendingQuietFlushSendWithExpectation(thread.ID, "queue:request", row, 0, 10, triage.PendingSendExpectation{})
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "preinit-tok"})
	// A closed store fails the stale row's cleanup, which requeues.
	if err := app.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	app.teardownDeadPreInitSession(thread.ID, "preinit-tok")

	requeued := app.triage.QueuedFlushItems(thread.ID)
	if len(requeued) != 1 {
		t.Fatalf("queued items after the teardown: got %d, want the request", len(requeued))
	}
	requireAgentPayload(t, "teardown", requeued[0].Payload)
	if wire := flushqueue.ItemFromTriage(thread.ID, requeued[0]); !wire.AgentMessage {
		t.Fatalf("the requeued request is listed as the person's message: %+v", wire)
	}
}

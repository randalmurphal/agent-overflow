package app

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

// TestClientSendsRefuseTheAppsSendIDs keeps a person's message from passing
// for one the app wrote: every public send entry refuses a send id the app
// would write, before it stores or sends anything, and takes any other.
func TestClientSendsRefuseTheAppsSendIDs(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("client-send-ids")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	entries := map[string]func(sendID string) error{
		"SendMessageWithOptions": func(id string) error {
			_, err := app.SendMessageWithOptions(ctx, thread.ID, "hello", SendMessageOptions{SendID: id})
			return err
		},
		"SteerMessageWithOptions": func(id string) error {
			_, err := app.SteerMessageWithOptions(ctx, thread.ID, "hello", SendMessageOptions{SendID: id})
			return err
		},
		"RegisterQueueItem": func(id string) error {
			_, err := app.RegisterQueueItem(ctx, thread.ID, "hello", SendMessageOptions{SendID: id})
			return err
		},
		"SubmitAsyncAnswers": func(id string) error {
			return app.SubmitAsyncAnswers(ctx, thread.ID, id, []store.AsyncQuestionAnswer{{ItemID: "q", Answer: "yes"}})
		},
		"RevertConversationAndResendMessage": func(id string) error {
			_, err := app.RevertConversationAndResendMessage(ctx, thread.ID, "user-item", RevertAndResendOptions{Content: "hello", SendID: id})
			return err
		},
	}
	// The ids the app writes, one of each kind.
	appIDs := []string{
		agentWakeSendID("wake-1", 1700000000000),
		agentSendIDRoot + "x",
		(&workerPrompt{token: "tok"}).sendID(),
		assistantSendID("opening", "a-1"),
		remoteCompletionSendID(store.RemoteWatch{ComputerID: "c", RequestID: "r"}),
		workflowWakeSendIDRoot + "run",
		threadRequestSendID("tok"),
		threadWakeSendID("tok"),
		threadWakeLateSendID("tok"),
	}
	for name, send := range entries {
		for _, id := range appIDs {
			if err := send(id); err == nil || !strings.Contains(err.Error(), "keeps for its own messages") {
				t.Errorf("%s(%q) = %v, want the app's send id refused", name, id, err)
			}
		}
	}
	if items, err := app.store.ListFlushQueueItems(thread.ID); err != nil || len(items) != 0 {
		t.Fatalf("queue after refused sends = %+v, %v; want nothing stored", items, err)
	}
	for _, id := range []string{"0b5f8a52-3c1e-4d4b-9f3e-2a7c1d9e8b6f", "async-answer:0b5f8a52-3c1e-4d4b-9f3e-2a7c1d9e8b6f", ""} {
		unlock, err := app.lockClientSendAdmission(ctx, thread.ID, id)
		if err != nil {
			t.Errorf("lockClientSendAdmission(%q) = %v, want a client's id taken", id, err)
			continue
		}
		unlock()
	}
}

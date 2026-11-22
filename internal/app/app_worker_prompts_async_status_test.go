package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"

	"github.com/google/uuid"
)

// The boot routes the async questions a restart left open: an app started
// on the database the previous one wrote, with nothing in memory, puts the
// worker's question to its spawner once its sweeps start.
func TestTheBootRoutesAnAsyncQuestionTheRestartLeftOpen(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.holdCallerSession()
	worker := f.settledSpawn(t, "boot-async-spawn", f.caller.ID)
	if _, err := f.app.store.RecordAsyncQuestions(store.Item{
		ID: "question:ask", ThreadID: worker.ID, Kind: "assistant_text", Role: "assistant", Status: "completed",
		Meta: `{"delivery":"async","questions":[{"title":"Which scope?"}]}`,
	}); err != nil {
		t.Fatal(err)
	}

	after := f.restart(t)
	if tokens := workerPromptTokens(t, after.app, f.caller.ID); len(tokens) != 0 {
		t.Fatalf("before the boot the spawner holds %q", tokens)
	}
	// The sweep runs for the app's life, so the request work group never
	// empties: poll the queue instead of waiting on it.
	after.app.startThreadRequestSweeps()
	waitUntil(t, 10*time.Second, func() bool { return len(workerPromptTokens(t, after.app, f.caller.ID)) == 1 })
	if detail, route, asking := after.app.asyncQuestionBlocked(worker.ID); !asking || detail != "Which scope?" || promptRouteTitle(route) != f.caller.Title {
		t.Fatalf("after the boot the worker asks %q of %q (%v), want its spawner", detail, promptRouteTitle(route), asking)
	}
}

// An idle worker with an open async question waits on it, and every reader
// of what a thread waits on says so alike (threadWaitingOn): its spawner's
// Agents row reads blocked, thread_status here and from a paired computer
// reads it awaiting input and not at rest, and the status a paired
// computer polls for a request on it reads blocked, with the same
// recipient, while the question is routed to the spawner and once it is
// handed back to the person.
func TestAWorkersOpenAsyncQuestionReadsAlikeEverywhere(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	dest := pair.dest
	dest.startSessionFn = func(string) error { return errors.New("the provider binary is missing") }
	spawner := reachThread(t, dest, uuid.NewString(), "Lead")
	worker := reachThread(t, dest, uuid.NewString(), "Release worker")
	spawn := uuid.NewString()
	if err := dest.store.InsertThreadRequest(store.ThreadRequest{
		Token: spawn, CallerThreadID: spawner.ID, TargetThreadID: worker.ID,
		Kind: store.ThreadRequestSpawn, State: store.ThreadRequestFinished,
	}); err != nil {
		t.Fatal(err)
	}
	owner := uuid.NewString()
	sent := uuid.NewString()
	if _, _, err := dest.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: sent, OwnerDeviceID: owner, Kind: store.ThreadRequestSend, TargetThreadID: worker.ID, State: store.ThreadReceiptAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	drain := func() {
		t.Helper()
		for range 3 {
			if err := dest.drainWorkerPrompts(context.Background(), 10*time.Second); err != nil {
				t.Fatal(err)
			}
			dest.waitAgentNoticeWork()
			// The Agents rows' reassessment of the worker.
			dest.threadRequestsWG.Wait()
		}
	}
	if _, err := dest.store.RecordAsyncQuestions(store.Item{
		ID: "question:ask", ThreadID: worker.ID, Kind: "assistant_text", Role: "assistant", Status: "completed",
		Meta: `{"delivery":"async","questions":[{"title":"Which scope?"}]}`,
	}); err != nil {
		t.Fatal(err)
	}
	dest.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": worker.ID})
	drain()

	agree := func(routedTo string) {
		t.Helper()
		listing, err := dest.ListThreadAgentRows(t.Context(), spawner.ID)
		if err != nil {
			t.Fatal(err)
		}
		row, found := agentRowByToken(listing.Rows, spawn)
		if !found || row.Worker != string(workerBlocked) || row.BlockedOn != "question" || row.BlockedDetail != "Which scope?" || row.BlockedRoutedTo != routedTo {
			t.Fatalf("the worker's Agents row = %+v (found %v), want blocked on its question routed to %q", row, found, routedTo)
		}
		here, err := (threadToolsApp{app: dest}).threadStateNow(t.Context(), worker.ID)
		if err != nil || here.State != threadtools.StateAwaitingInput || here.Resting {
			t.Fatalf("thread_status on its computer = %+v, %v; want it awaiting input", here, err)
		}
		result := pair.call(t, "thread_status", `{"thread_ids":["`+worker.ID+`"]}`)
		threads, _ := result["threads"].([]any)
		if len(threads) != 1 {
			t.Fatalf("thread_status from the paired computer = %+v", result)
		}
		there, _ := threads[0].(map[string]any)
		if resting, _ := there["resting"].(bool); resting || there["state"] != threadtools.StateAwaitingInput {
			t.Fatalf("thread_status from the paired computer = %+v; want it awaiting input", there)
		}
		peer, err := dest.threadPeerRequestState(t.Context(), owner, sent)
		if err != nil || !peer.Blocked || peer.BlockedRoutedTo != routedTo {
			t.Fatalf("the polled status of a request on it = %+v, %v; want blocked, routed to %q", peer, err, routedTo)
		}
	}
	agree(spawner.Title)

	var token string
	for _, row := range durableQueueRows(t, dest, spawner.ID) {
		if strings.HasPrefix(row.SendID, workerPromptSendIDRoot) {
			token = row.SendID
		}
	}
	if token == "" {
		t.Fatal("the spawner holds no prompt wake")
	}
	if stop, err := dest.StopThreadAgentIncoming(t.Context(), spawner.ID, token); err != nil || stop.Outcome != threadAgentStopped {
		t.Fatalf("withdraw = %+v, %v", stop, err)
	}
	drain()
	agree("")
}

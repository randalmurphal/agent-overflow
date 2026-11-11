package app

import (
	"context"
	"slices"
	"sync"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// A spawn whose caller stops waiting once the spawned thread exists still
// sends that thread its prompt. A retry with the same token never creates
// or dispatches again, so a spawn abandoned there would leave a thread with
// no work in it and a request that can never run. The same holds for a
// spawn a paired computer forwarded, whose caller is its connection.
func TestThreadSpawnCancelledAfterItsThreadExistsStillSendsThePrompt(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "done")
	before := threadIDsInStore(t, f.app)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var once sync.Once
	f.app.testEmitHook = func(name string, data any) {
		event, ok := data.(triage.ThreadUpdateEvent)
		if name != string(eventchan.ThreadUpdated) || !ok || event.Thread == nil || slices.Contains(before, event.Thread.ID) {
			return
		}
		// The spawned thread was just created: cancel between creating it
		// and sending it the prompt.
		once.Do(cancel)
	}
	ack, err := f.adapter().Spawn(ctx, f.callerIdentity(), threadtools.SpawnCall{Prompt: "check the build", Notify: true})

	after := threadIDsInStore(t, f.app)
	if err != nil {
		// Not started: nothing may be left behind.
		if len(after) != len(before) {
			t.Fatalf("a refused spawn (%v) left threads %v, started with %v", err, after, before)
		}
		return
	}
	row := f.userRow(t, ack.ThreadID, ack.Token)
	if row.ID == "" {
		t.Fatal("the spawned thread never received its prompt")
	}
	if receipt := f.receipt(t, ack.Token); receipt.State == store.ThreadReceiptErrored || receipt.State == store.ThreadReceiptInterrupted {
		t.Fatalf("receipt = %+v, want the request running or finished", receipt)
	}
}

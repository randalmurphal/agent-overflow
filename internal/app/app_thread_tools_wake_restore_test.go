package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// A wake is the app's message to the agent, never the person's: every path
// that takes queued messages back to the composer gives a wake among them
// back to its requests instead (giveBackToOwners), owed again and disarmed,
// and the composer gets only the person's text. thread_status, or a later
// wait, is where the answer is read. A caller whose session cannot start
// keeps the wake queued for its next start, as every notice does. The boot
// restore is pinned by TestOneTurnAnsweringTwoRequestsWakesTheirSenderOnce,
// and a start that leaves no session by
// TestAParkedTrialSettlesTheLastRunsRequestsButActsOnlyAfterItsCommit.

// queuedWake is a settled request whose wake sits in the caller's queue,
// with dispatch held so nothing sends it.
func (f *requestFixture) queuedWake(t *testing.T, answer string) string {
	t.Helper()
	holdFlushQueue(f.app)
	f.holdCallerStart()
	token := f.settledNotifyRequest(t, answer)
	f.app.deliverThreadWake(token, false)
	waitUntil(t, 10*time.Second, func() bool { return f.request(t, token).DeliveredHow == store.ThreadWakeQueued })
	return token
}

// requireWakeGivenBack asserts the wake's message is in neither the
// caller's composer nor its queue, and its request is owed again and
// disarmed.
func (f *requestFixture) requireWakeGivenBack(t *testing.T, token, answer string) {
	t.Helper()
	f.requireWakeOutOfTheComposer(t, answer)
	for _, row := range durableQueueRows(t, f.app, f.caller.ID) {
		if row.SendID == threadWakeSendID(token) {
			t.Fatalf("the wake is still queued: %+v", row)
		}
	}
	if row := f.request(t, token); row.DeliveredHow != "" || row.DeliveredAt != 0 || row.Notify {
		t.Fatalf("request = delivered %q at %d, notify %v; want it owed again and disarmed", row.DeliveredHow, row.DeliveredAt, row.Notify)
	}
}

// requireWakeOutOfTheComposer asserts the caller's composer does not carry
// the wake's answer.
func (f *requestFixture) requireWakeOutOfTheComposer(t *testing.T, answer string) {
	t.Helper()
	draft, _, err := f.app.store.GetThreadDraft(f.caller.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if strings.Contains(draft.Content, answer) {
		t.Fatalf("composer = %q, want the wake carrying %q kept out of it", draft.Content, answer)
	}
}

// A caller with no session whose start fails keeps its wake queued for the
// next start, with the failure on the thread; the composer does not get it.
func TestAWakeReturnedBecauseTheCallerCannotStartReadsDraft(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	caller := f.caller.ID
	f.app.configureTriageQueueCallbacks()
	f.app.startSessionFn = func(threadID string) error {
		if threadID == caller {
			return errors.New("the session cannot start")
		}
		return f.app.startSessionNow(threadID)
	}
	token := f.settledNotifyRequest(t, "the build is green")
	f.app.deliverThreadWake(token, false)
	f.app.waitAgentNoticeWork()

	if row := f.request(t, token); row.DeliveredHow != store.ThreadWakeQueued || !row.Notify {
		t.Fatalf("request = delivered %q, notify %v; want its wake still queued", row.DeliveredHow, row.Notify)
	}
	queued := false
	for _, row := range durableQueueRows(t, f.app, caller) {
		queued = queued || row.SendID == threadWakeSendID(token)
	}
	if !queued {
		t.Fatal("the wake left the caller's queue")
	}
	if rows := threadErrorRows(t, f.app.store, caller, ""); len(rows) == 0 {
		t.Fatal("the failed start left no row saying why nothing answered")
	}
	f.requireWakeOutOfTheComposer(t, "the build is green")
}

// A session that dies, or that the app stops for a reason other than the
// person's Stop, hands its queue back to the composer, and a wake in it back
// to its request.
func TestAWakeRestoredWhenTheCallersSessionEndsReadsDraft(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "noted")
	token := f.queuedWake(t, "the migration is reversible")

	f.app.restoreUnconfirmedQueueOnSessionDeath(f.caller.ID)

	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("queue after the restore = %+v, want it emptied", rows)
	}
	f.requireWakeGivenBack(t, token, "the migration is reversible")
}

// A wake written to Claude that Claude cancels without running goes back to
// its request from the row it was written as, which names its send id.
func TestAWakeClaudeCancelledUnrunReadsDraft(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "noted")
	token := f.queuedWake(t, "the lint is clean")
	f.app.configureTriageQueueCallbacks()
	caller := f.caller.ID
	rows := durableQueueRows(t, f.app, caller)
	if len(rows) != 1 {
		t.Fatalf("queue = %+v, want the wake", rows)
	}
	if _, taken := f.app.triage.RemoveQueuedFlushItem(caller, rows[0].ID); !taken {
		t.Fatal("the wake was not on the in-memory queue")
	}
	meta, err := json.Marshal(usermessage.Meta{SendID: threadWakeSendID(token)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	row := store.Item{ID: "user:flush:wake", ThreadID: caller, Kind: "user_text", Role: "user", Status: "completed",
		Summary: rows[0].Message, Meta: string(meta), CreatedAt: now, UpdatedAt: now}
	f.app.triage.RegisterPendingFlushSendWithExpectation(caller, rows[0].ID, row, rows[0].EnqueuedAt,
		triage.PendingSendExpectation{ProviderItemID: "u-wake"})
	for _, state := range []provider.CommandLifecycleState{provider.CommandQueued, provider.CommandCancelled} {
		lifecycle, err := json.Marshal(provider.CommandLifecycleMeta{CommandUUID: "u-wake", State: state})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.app.triage.Handle(provider.ProviderEvent{Kind: provider.EventCommandLifecycle, ThreadID: caller,
			ItemID: "u-wake", Meta: lifecycle, Timestamp: time.Now()}); err != nil {
			t.Fatalf("lifecycle %s: %v", state, err)
		}
	}

	waitUntil(t, 10*time.Second, func() bool { return len(durableQueueRows(t, f.app, caller)) == 0 })
	waitUntil(t, 10*time.Second, func() bool { return f.request(t, token).DeliveredAt == 0 })
	f.requireWakeGivenBack(t, token, "the lint is clean")
}

// A message the provider had and lost (a failed Codex resend, a row taken
// back out of the Codex queue) is restored from its timeline row, which
// names its send id in its meta: a wake goes back to its request.
func TestAWakeRestoredFromItsTimelineRowReadsDraft(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "noted")
	token := f.queuedWake(t, "the cache was cold")
	meta, err := json.Marshal(usermessage.Meta{SendID: threadWakeSendID(token)})
	if err != nil {
		t.Fatal(err)
	}

	f.app.restoreEagerPersistedFlushesToDraft(f.caller.ID, []triage.EagerPersistedFlush{{
		UserItemID: "wake-row-gone", Content: "the cache was cold", Meta: string(meta),
	}})

	f.requireWakeOutOfTheComposer(t, "the cache was cold")
	if row := f.request(t, token); row.DeliveredHow != "" || row.DeliveredAt != 0 || row.Notify {
		t.Fatalf("request = delivered %q at %d, notify %v; want it owed again and disarmed", row.DeliveredHow, row.DeliveredAt, row.Notify)
	}
}

package app

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// thread_status says a wake is still on its way until the provider's echo
// consumed it. The echo confirms the delivery before triage stamps the
// caller's timeline row, and either write can fail on its own: what decides
// is the confirmation, or the echo kept because its confirmation did not
// write, never the timeline row.

// echoWake is the provider's echo of the message envelope carried.
func echoWake(t *testing.T, f *requestFixture, envelope map[string]any) {
	t.Helper()
	wireID, _ := envelope["uuid"].(string)
	text, _ := envelopeText(t, envelope)
	meta, _ := json.Marshal(map[string]any{"provider_item_id": wireID})
	// A refused write is the case under test; the echo reports it.
	_ = f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: f.caller.ID, Content: text, Meta: meta, Timestamp: time.Now(),
	})
}

func TestAConsumedWakeIsNotReportedQueued(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// refuse is the trigger that fails one of the echo's writes.
		refuse string
		// confirmed is whether the confirmation lands.
		confirmed bool
	}{
		{"the timeline stamp failed", `CREATE TRIGGER refuse_stamp BEFORE UPDATE ON items
			WHEN NEW.kind = 'user_text' AND instr(NEW.meta, 'provider_item_id') > 0
			BEGIN SELECT RAISE(ABORT, 'injected stamp failure'); END`, true},
		{"the confirmation failed", `CREATE TRIGGER refuse_confirm BEFORE UPDATE OF confirmed_at ON thread_requests
			BEGIN SELECT RAISE(ABORT, 'injected confirmation failure'); END`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.app.startSessionFn = func(string) error { return nil }
			carrier, rider, envelope := queueFoldedWake(t, f)
			for _, token := range []string{carrier, rider} {
				if !f.adapter().wakeQueued(f.request(t, token), false) {
					t.Fatalf("%s: a message handed to the provider and not echoed is not reported queued", token)
				}
			}
			execOnStore(t, f.dbPath, tc.refuse)
			echoWake(t, f, envelope)
			for _, token := range []string{carrier, rider} {
				row := f.request(t, token)
				if confirmed := row.ConfirmedAt != 0; confirmed != tc.confirmed {
					t.Fatalf("fixture: %s confirmed %v, want %v", token, confirmed, tc.confirmed)
				}
				if f.adapter().wakeQueued(row, false) {
					t.Errorf("%s: a wake the provider consumed is reported still queued", token)
				}
			}
		})
	}
}

// A late reply's wake reads its own confirmation.
func TestAConsumedLateReplyIsNotReportedQueued(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	var token string
	for _, seed := range owedWakeSeeds() {
		if seed.late {
			token = seed.seed(t, f)
		}
	}
	f.app.deliverThreadWake(token, true)
	f.app.waitAgentNoticeWork()
	envelope := dispatchUnconfirmed(t, f)
	if !f.adapter().wakeQueued(f.request(t, token), true) {
		t.Fatal("a late reply handed to the provider and not echoed is not reported queued")
	}
	execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_stamp BEFORE UPDATE ON items
		WHEN NEW.kind = 'user_text' AND instr(NEW.meta, 'provider_item_id') > 0
		BEGIN SELECT RAISE(ABORT, 'injected stamp failure'); END`)
	echoWake(t, f, envelope)
	row := f.request(t, token)
	if row.LateConfirmedAt == 0 {
		t.Fatal("fixture: the late reply's echo did not confirm it")
	}
	if f.adapter().wakeQueued(row, true) {
		t.Error("a late reply the provider consumed is reported still queued")
	}
	if f.adapter().wakeQueued(row, false) {
		t.Error("the first answer, delivered inline, is reported queued")
	}
}

// A wake given back to its request is not on its way.
func TestAGivenBackWakeIsNotReportedQueued(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	carrier, rider, _ := queueFoldedWake(t, f)
	f.app.giveBackToOwners(f.caller.ID, []ownedMessage{{sendID: threadWakeSendID(carrier), unconfirmed: true}})
	for _, token := range []string{carrier, rider} {
		if f.adapter().wakeQueued(f.request(t, token), false) {
			t.Errorf("%s: a wake given back is reported queued", token)
		}
	}
}

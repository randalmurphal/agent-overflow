package app

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// queueFoldedWake settles two of the caller's requests with the same answer,
// delivers their wakes as one message and hands it to the provider without
// an echo. It returns the carrier, the rider and the envelope written.
func queueFoldedWake(t *testing.T, f *requestFixture) (carrier, rider string, envelope map[string]any) {
	t.Helper()
	carrier = settledAnswerHolding(t, f, "the same answer")
	rider = settledAnswerHolding(t, f, "the same answer")
	if row := f.request(t, carrier); row.CreatedAt > f.request(t, rider).CreatedAt {
		carrier, rider = rider, carrier
	}
	f.app.deliverThreadWakes([]threadWakeRef{{token: carrier}, {token: rider}})
	f.app.waitAgentNoticeWork()
	if row := f.request(t, rider); row.DeliveredWith != carrier || row.DeliveredHow != store.ThreadWakeQueued {
		t.Fatalf("rider = delivered %q with %q, want it in the carrier's message", row.DeliveredHow, row.DeliveredWith)
	}
	return carrier, rider, dispatchUnconfirmed(t, f)
}

// The provider echoes one message for both requests, under the carrier's
// send id: the echo confirms the rider with the carrier, on the same row, and
// a restart gives back neither.
func TestTheEchoOfAFoldedWakeConfirmsEveryRequestItAnswers(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	carrier, rider, envelope := queueFoldedWake(t, f)

	wireID, _ := envelope["uuid"].(string)
	text, _ := envelopeText(t, envelope)
	meta, _ := json.Marshal(map[string]any{"provider_item_id": wireID})
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: f.caller.ID, Content: text, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	carrierRow, riderRow := f.request(t, carrier), f.request(t, rider)
	if carrierRow.ConfirmedAt == 0 || riderRow.ConfirmedAt != carrierRow.ConfirmedAt {
		t.Fatalf("confirmed at carrier %d rider %d, want the rider confirmed with its carrier", carrierRow.ConfirmedAt, riderRow.ConfirmedAt)
	}
	deliveries, err := f.app.store.ThreadRequestDeliveries(f.caller.ID, []string{carrier, rider})
	if err != nil || len(deliveries) != 2 || deliveries[0].WakeItemID == "" || deliveries[1].WakeItemID != deliveries[0].WakeItemID {
		t.Fatalf("deliveries = %+v, %v; want both on the row that carried the message", deliveries, err)
	}

	after := f.restart(t)
	after.app.restoreDurableFlushQueueAtBoot()
	for _, token := range []string{carrier, rider} {
		if row := after.request(t, token); !threadRequestDelivered(row, false) {
			t.Errorf("the boot sweep gave back %s, which the echo confirmed", token)
		}
	}
}

// A restart finds the folded message handed to the provider and never
// echoed: the boot sweep gives it back once, by its carrier, and the rider
// goes back with it, owed again and disarmed.
func TestTheBootSweepGivesBackEveryRequestAnUnconfirmedFoldedWakeAnswered(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	carrier, rider, _ := queueFoldedWake(t, f)

	after := f.restart(t)
	after.app.restoreDurableFlushQueueAtBoot()
	for _, token := range []string{carrier, rider} {
		requireOwedAgain(t, after, token, false)
		if row := after.request(t, token); row.DeliveredWith != "" {
			t.Errorf("%s still records carrier %q after the give-back", token, row.DeliveredWith)
		}
	}
}

// A Stop that finds the folded message never echoed gives back the rider
// with its carrier.
func TestAStopGivesBackEveryRequestAnUnconfirmedFoldedWakeAnswered(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	carrier, rider, _ := queueFoldedWake(t, f)

	f.app.giveBackToOwners(f.caller.ID, []ownedMessage{{sendID: threadWakeSendID(carrier), unconfirmed: true}})
	for _, token := range []string{carrier, rider} {
		requireOwedAgain(t, f, token, false)
	}
}

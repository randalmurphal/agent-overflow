package itemmeta

import "testing"

// The rows below are the meta an earlier version persisted, spelled as it
// sits in the database, so a key renamed in the reader alone fails here.
const (
	// Stamped before `thread/queue/add`: the hand-off is unproven.
	unprovenQueuedRow = `{"providerQueued":true,"providerQueueHandoff":true}`
	// The same row once the add was acked: an explicit false.
	confirmedQueuedRow = `{"providerQueued":true,"providerQueueHandoff":false}`
	// The resume-side re-arm, which marked a row the queue just named.
	reArmedQueuedRow = `{"providerQueued":true}`
	// An unproven hand-off on a row that carried user-message meta.
	unprovenQueuedRowWithMeta = `{"attachments":[{"id":"a1"}],"providerQueueHandoff":true,"providerQueued":true,"provider_item_id":""}`
)

// TestProviderQueueReadersTellAnUnprovenAddFromADispatchedOne pins the
// distinction the marker exists for.
//
// Both states mean "this row's message went to the provider's queue", so
// both read as provider-queued. Only the unproven one reads as pending, and
// that decides what a later absence from the provider's queue means: a
// dispatched message (leave the row as history) or an add that never landed
// (give the message back). Collapse them and a message the provider never
// took is stranded forever, because the marker makes every recovery path
// step around its row.
func TestProviderQueueReadersTellAnUnprovenAddFromADispatchedOne(t *testing.T) {
	for _, tc := range []struct {
		name            string
		raw             string
		queued, pending bool
	}{
		{"unproven", unprovenQueuedRow, true, true},
		{"unproven with user-message meta", unprovenQueuedRowWithMeta, true, true},
		{"confirmed", confirmedQueuedRow, true, false},
		{"re-armed", reArmedQueuedRow, true, false},
		{"unmarked", `{"attachments":[{"id":"a1"}]}`, false, false},
		{"empty", "", false, false},
		// Corrupt meta reads as unmarked: the direction that keeps the
		// message recoverable by hand.
		{"malformed", "not json", false, false},
	} {
		if got := IsProviderQueued(tc.raw); got != tc.queued {
			t.Errorf("%s: IsProviderQueued = %v, want %v", tc.name, got, tc.queued)
		}
		if got := IsProviderQueueHandoffPending(tc.raw); got != tc.pending {
			t.Errorf("%s: IsProviderQueueHandoffPending = %v, want %v", tc.name, got, tc.pending)
		}
	}
}

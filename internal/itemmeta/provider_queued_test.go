package itemmeta

import (
	"encoding/json"
	"testing"
)

// TestProviderQueueHandoffSeparatesAnUnprovenAddFromADispatchedOne pins the
// distinction the whole marker exists for.
//
// Both states mean "this row's message went to the provider's queue", so both
// keep every reader of `providerQueued` working. What only the second state
// can say is that the hand-off was PROVEN, and that is what decides what a
// later absence from the provider's queue means: a dispatched message (leave
// the row as history) or an add that never landed (give the message back).
// Collapse them and a message the provider never took is stranded forever,
// because the marker makes every recovery path step around its row.
func TestProviderQueueHandoffSeparatesAnUnprovenAddFromADispatchedOne(t *testing.T) {
	unproven, err := MarkProviderQueueHandoff("")
	if err != nil {
		t.Fatalf("MarkProviderQueueHandoff: %v", err)
	}
	if !IsProviderQueued(unproven) {
		t.Error("an unproven hand-off is not provider-queued; the session-death drain would restore a message the provider may already own")
	}
	if !IsProviderQueueHandoffPending(unproven) {
		t.Error("an unproven hand-off does not read as pending; nothing could ever tell it from a dispatched row")
	}

	proven, err := ConfirmProviderQueueHandoff(unproven)
	if err != nil {
		t.Fatalf("ConfirmProviderQueueHandoff: %v", err)
	}
	if !IsProviderQueued(proven) {
		t.Error("confirming the hand-off dropped the provider-queued marker")
	}
	if IsProviderQueueHandoffPending(proven) {
		t.Error("a confirmed hand-off still reads as pending; the row would be handed back to the composer after it ran")
	}

	// The resume-side re-arm marks a row the queue just named, which is proof
	// on its own.
	direct, err := MarkProviderQueued("")
	if err != nil {
		t.Fatalf("MarkProviderQueued: %v", err)
	}
	if !IsProviderQueued(direct) || IsProviderQueueHandoffPending(direct) {
		t.Errorf("MarkProviderQueued = %q, want provider-queued and proven", direct)
	}
}

// TestProviderQueueHandoffWritesBothKeysWithoutLosingTheRestOfTheMeta guards
// the merge itself: the marker is stamped onto a row that already carries the
// user-message meta (attachments, plan provenance), and it is written in ONE
// decode/encode so a row can never persist as queued-but-not-pending.
func TestProviderQueueHandoffWritesBothKeysWithoutLosingTheRestOfTheMeta(t *testing.T) {
	raw, err := MarkProviderQueueHandoff(`{"attachments":[{"id":"a1"}],"provider_item_id":""}`)
	if err != nil {
		t.Fatalf("MarkProviderQueueHandoff: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("unmarshal merged meta: %v", err)
	}
	if _, ok := decoded["attachments"]; !ok {
		t.Errorf("merged meta = %q, want the row's existing keys preserved", raw)
	}
	if decoded[providerQueuedKey] != true || decoded[providerQueueHandoffKey] != true {
		t.Errorf("merged meta = %q, want both hand-off keys set in one write", raw)
	}

	if _, err := MarkProviderQueueHandoff("not json"); err == nil {
		t.Error("malformed meta merged silently; the caller is about to persist the row that IS the ownership record")
	}
}

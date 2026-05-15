package flushqueue

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

func TestItemFromTriageNilPayload(t *testing.T) {
	got := ItemFromTriage("thread-1", triage.QueuedFlushItem{
		ID:         "queue:abc",
		Message:    "hello",
		EnqueuedAt: 123,
	})
	if got.ID != "queue:abc" || got.ThreadID != "thread-1" || got.Message != "hello" || got.EnqueuedAt != 123 {
		t.Fatalf("identity fields wrong: %+v", got)
	}
	if got.AttachmentIDs != nil || got.SourceProposedPlan != nil || got.RevisionSourceProposedPlan != nil {
		t.Fatalf("optional fields populated when payload is empty: %+v", got)
	}
	if got.RevisionSourceCommentIDs != nil || got.RevisionSourceDiffReview != nil || got.RevisionSourceDiffCommentIDs != nil {
		t.Fatalf("revision fields populated when payload is empty: %+v", got)
	}
}

func TestItemFromTriagePopulatedPayload(t *testing.T) {
	plan := &store.ProposedPlanSourceRef{ItemID: "plan-1", Title: "plan title"}
	diff := &store.DiffReviewSourceRef{Scope: "scope-1", SourceKey: "diff-key"}
	payload := Payload{
		AttachmentIDs:                []string{"att-1", "att-2"},
		SourceProposedPlan:           plan,
		RevisionSourceProposedPlan:   plan,
		RevisionSourceCommentIDs:     []string{"c-1"},
		RevisionSourceDiffReview:     diff,
		RevisionSourceDiffCommentIDs: []string{"d-1", "d-2"},
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := ItemFromTriage("thread-2", triage.QueuedFlushItem{
		ID:         "queue:def",
		Message:    "stage edits",
		EnqueuedAt: 456,
		Payload:    rawPayload,
	})
	if got.ID != "queue:def" || got.ThreadID != "thread-2" || got.Message != "stage edits" || got.EnqueuedAt != 456 {
		t.Fatalf("identity fields wrong: %+v", got)
	}
	if len(got.AttachmentIDs) != 2 || got.AttachmentIDs[0] != "att-1" || got.AttachmentIDs[1] != "att-2" {
		t.Fatalf("AttachmentIDs = %v, want [att-1 att-2]", got.AttachmentIDs)
	}
	if got.SourceProposedPlan == nil || got.SourceProposedPlan.ItemID != "plan-1" {
		t.Fatalf("SourceProposedPlan = %+v, want ItemID plan-1", got.SourceProposedPlan)
	}
	if got.RevisionSourceProposedPlan == nil || got.RevisionSourceProposedPlan.ItemID != "plan-1" {
		t.Fatalf("RevisionSourceProposedPlan = %+v", got.RevisionSourceProposedPlan)
	}
	if got.RevisionSourceDiffReview == nil || got.RevisionSourceDiffReview.Scope != "scope-1" || got.RevisionSourceDiffReview.SourceKey != "diff-key" {
		t.Fatalf("RevisionSourceDiffReview = %+v", got.RevisionSourceDiffReview)
	}
	if len(got.RevisionSourceCommentIDs) != 1 || got.RevisionSourceCommentIDs[0] != "c-1" {
		t.Fatalf("RevisionSourceCommentIDs = %v", got.RevisionSourceCommentIDs)
	}
	if len(got.RevisionSourceDiffCommentIDs) != 2 || got.RevisionSourceDiffCommentIDs[1] != "d-2" {
		t.Fatalf("RevisionSourceDiffCommentIDs = %v", got.RevisionSourceDiffCommentIDs)
	}
}

func TestItemFromTriageCorruptPayloadReturnsIdentityOnly(t *testing.T) {
	// A corrupt payload must still produce a renderable wire item:
	// losing the attachment refs is preferable to dropping the message
	// entirely while it waits above the composer.
	got := ItemFromTriage("thread-3", triage.QueuedFlushItem{
		ID:         "queue:ghi",
		Message:    "broken payload",
		EnqueuedAt: 789,
		Payload:    []byte("{not json"),
	})
	if got.ID != "queue:ghi" || got.ThreadID != "thread-3" || got.Message != "broken payload" || got.EnqueuedAt != 789 {
		t.Fatalf("identity fields wrong: %+v", got)
	}
	if got.AttachmentIDs != nil || got.SourceProposedPlan != nil || got.RevisionSourceProposedPlan != nil {
		t.Fatalf("payload fields populated on decode failure: %+v", got)
	}
}

func TestNewItemIDPrefix(t *testing.T) {
	id := NewItemID()
	if !strings.HasPrefix(id, "queue:") {
		t.Fatalf("NewItemID() = %q, want prefix %q", id, "queue:")
	}
	if id == "queue:" {
		t.Fatalf("NewItemID() returned empty uuid")
	}
	// Collisions are statistically impossible; assert distinctness as a
	// sanity check.
	if NewItemID() == id {
		t.Fatalf("NewItemID returned the same id twice")
	}
}

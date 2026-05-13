package flushqueue

import (
	"encoding/json"
	"log"

	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

// QueuedItem is the wire-side projection of a triage QueuedFlushItem,
// used by the frontend to mirror the backend's per-thread queue. The
// wire shape mirrors SendMessageOptions's data fields plus the
// frontend-allocated id and stamped enqueuedAt — together they're
// enough for both the queue overlay rendering and the UP-arrow
// retract path that re-hydrates the composer draft.
//
// AttachmentIDs (not full Attachment records) ride the wire because
// the frontend already has the full records in its attachment store
// keyed by id; cross-wire transmission would duplicate bytes for no
// gain. Plan refs are passed by value because they're tiny and
// already used as plain JSON across the existing send path.
type QueuedItem struct {
	ID                           string                        `json:"id"`
	ThreadID                     string                        `json:"threadId"`
	Message                      string                        `json:"message"`
	AttachmentIDs                []string                      `json:"attachmentIds,omitempty"`
	SourceProposedPlan           *store.ProposedPlanSourceRef  `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan   *store.ProposedPlanSourceRef  `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs     []string                      `json:"revisionSourceCommentIds,omitempty"`
	RevisionSourceDiffReview     *store.DiffReviewSourceRef    `json:"revisionSourceDiffReview,omitempty"`
	RevisionSourceDiffCommentIDs []string                      `json:"revisionSourceDiffCommentIds,omitempty"`
	EnqueuedAt                   int64                         `json:"enqueuedAt"`
}

// Payload is the wire shape of triage.QueuedFlushItem.Payload — the
// inner JSON the frontend writes via RegisterQueueItem and that the
// dispatcher decodes when the flush trigger fires. Mirrors the data
// fields on sendMessageOptions except RuntimeMode — by the time the
// flush fires, the round's runtime mode is already established and a
// mid-round flip would defeat the whole point of in-flight queueing.
type Payload struct {
	AttachmentIDs                []string                     `json:"attachmentIds,omitempty"`
	SourceProposedPlan           *store.ProposedPlanSourceRef `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan   *store.ProposedPlanSourceRef `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs     []string                     `json:"revisionSourceCommentIds,omitempty"`
	RevisionSourceDiffReview     *store.DiffReviewSourceRef   `json:"revisionSourceDiffReview,omitempty"`
	RevisionSourceDiffCommentIDs []string                     `json:"revisionSourceDiffCommentIds,omitempty"`
}

// ItemFromTriage decodes a triage QueuedFlushItem back into the
// wire-side QueuedItem. The Payload is opaque app-layer JSON; on
// decode failure we still return a partially-populated wire item so
// the frontend can render the message text and offer retract — losing
// attachment refs on a corrupt payload is preferable to the wire
// dropping the item entirely.
func ItemFromTriage(threadID string, item triage.QueuedFlushItem) QueuedItem {
	out := QueuedItem{
		ID:         item.ID,
		ThreadID:   threadID,
		Message:    item.Message,
		EnqueuedAt: item.EnqueuedAt,
	}
	if len(item.Payload) == 0 {
		return out
	}
	var payload Payload
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		log.Printf("decode queued item payload thread=%s item=%s: %v", threadID, item.ID, err)
		return out
	}
	out.AttachmentIDs = payload.AttachmentIDs
	out.SourceProposedPlan = payload.SourceProposedPlan
	out.RevisionSourceProposedPlan = payload.RevisionSourceProposedPlan
	out.RevisionSourceCommentIDs = payload.RevisionSourceCommentIDs
	out.RevisionSourceDiffReview = payload.RevisionSourceDiffReview
	out.RevisionSourceDiffCommentIDs = payload.RevisionSourceDiffCommentIDs
	return out
}

// NewItemID allocates a new opaque queue-item id. The `queue:` prefix
// matches the frontend's draft-id convention so the id is recognisable
// in logs / traces. The uuid suffix carries the uniqueness — collision
// against another concurrent register on the same thread is
// statistically impossible.
func NewItemID() string {
	return "queue:" + uuid.NewString()
}

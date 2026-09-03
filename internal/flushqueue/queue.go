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
// enough for queue overlay rendering and provider-echo correlation.
//
// AttachmentIDs (not full Attachment records) ride the wire because
// the frontend already has the full records in its attachment store
// keyed by id; cross-wire transmission would duplicate bytes for no
// gain. Plan refs are passed by value because they're tiny and
// already used as plain JSON across the existing send path.
type QueuedItem struct {
	ID                           string                       `json:"id"`
	ThreadID                     string                       `json:"threadId"`
	Message                      string                       `json:"message"`
	AttachmentIDs                []string                     `json:"attachmentIds,omitempty"`
	SourceProposedPlan           *store.ProposedPlanSourceRef `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan   *store.ProposedPlanSourceRef `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs     []string                     `json:"revisionSourceCommentIds,omitempty"`
	RevisionSourceDiffReview     *store.DiffReviewSourceRef   `json:"revisionSourceDiffReview,omitempty"`
	RevisionSourceDiffCommentIDs []string                     `json:"revisionSourceDiffCommentIds,omitempty"`
	EnqueuedAt                   int64                        `json:"enqueuedAt"`
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
	// ExpandComposerCommands distinguishes a person's composer message from
	// app-injected prose. It is resolved at dispatch time, after a queued
	// message has waited out the active turn.
	ExpandComposerCommands bool `json:"expandComposerCommands,omitempty"`
	// SendID is the client-minted idempotency id of the composer send that
	// queued this message, or "" for an app-internal injector. It rides the
	// payload so the dispatcher can stamp it onto the `user_text` row it
	// persists: the durable queue row (`store.FlushQueueItem`) carries the
	// id while the message waits, and the item carries it afterwards, so
	// the two together cover the whole life of one send with no gap for a
	// re-sent frame to fall into. See `usermessage.Meta.SendID` for why the
	// message itself is the record.
	SendID string `json:"sendId,omitempty"`
}

// ItemFromTriage decodes a triage QueuedFlushItem back into the
// wire-side QueuedItem. The Payload is opaque app-layer JSON; on
// decode failure we still return a partially-populated wire item so
// the frontend can render the message text — losing attachment refs on
// a corrupt payload is preferable to dropping the item entirely.
func ItemFromTriage(threadID string, item triage.QueuedFlushItem) QueuedItem {
	return itemFrom(threadID, item.ID, item.Message, item.EnqueuedAt, item.Payload)
}

// ItemFromStore is the same projection from the DURABLE queue row. The two
// exist because a queued message has two homes over its life — the process's
// triage state while a session is running, and `flush_queue_items` from the
// moment it is registered until it reaches a durable endpoint — and a reader
// that found it in one must be able to answer in the same wire shape as a
// reader that found it in the other.
func ItemFromStore(item store.FlushQueueItem) QueuedItem {
	return itemFrom(item.ThreadID, item.ID, item.Message, item.EnqueuedAt, item.Payload)
}

func itemFrom(threadID, id, message string, enqueuedAt int64, raw []byte) QueuedItem {
	out := QueuedItem{
		ID:         id,
		ThreadID:   threadID,
		Message:    message,
		EnqueuedAt: enqueuedAt,
	}
	if len(raw) == 0 {
		return out
	}
	var payload Payload
	if err := json.Unmarshal(raw, &payload); err != nil {
		log.Printf("decode queued item payload thread=%s item=%s: %v", threadID, id, err)
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

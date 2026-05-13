// Package usermessage owns the JSON wire shape persisted in
// store.Item.Meta for user-authored timeline items, plus the
// marshal / unmarshal helpers that every entry point (send, steer,
// flush, fork, revert-to-draft) routes through.
//
// The shape is what the frontend reads back when rendering the user
// row's attachments, source-plan badge, and revision-context badges;
// changes here change what the UI sees, so the JSON tags are part of
// the contract.
package usermessage

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
)

// Meta is the JSON shape stored in store.Item.Meta for user_text rows.
// All fields are `omitempty` so a meta row with no attachments and no
// plan / diff revision context serialises to an empty string — the
// caller can then store SQL NULL or skip the Meta write entirely.
type Meta struct {
	Attachments                  []AttachmentMeta                  `json:"attachments,omitempty"`
	SourceProposedPlan           *store.ProposedPlanSourceRef      `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan   *store.ProposedPlanSourceRef      `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs     []string                          `json:"revisionSourceCommentIds,omitempty"`
	RevisionSourceDiffReview     *store.DiffReviewSourceRef        `json:"revisionSourceDiffReview,omitempty"`
	RevisionSourceDiffCommentIDs []string                          `json:"revisionSourceDiffCommentIds,omitempty"`
}

// AttachmentMeta is the per-attachment slice element. The Go side
// projects from store.Attachment into this minimal shape so the
// frontend doesn't see internal columns (storage paths, raw hashes,
// timestamps) that aren't relevant for rendering.
type AttachmentMeta struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
	Filename string `json:"filename"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
}

// Marshal returns the JSON-encoded user-message meta for the given
// attachments + plan/diff revision context. When every input is
// zero-valued the function returns ("", nil) so callers can persist
// an empty Meta column and the frontend's omit-empty branches
// continue to work.
func Marshal(
	attachments []store.Attachment,
	sourcePlan, revisionSourcePlan *store.ProposedPlanSourceRef,
	revisionCommentIDs []string,
	revisionSourceDiff *store.DiffReviewSourceRef,
	revisionDiffCommentIDs []string,
) (string, error) {
	if len(attachments) == 0 &&
		sourcePlan == nil &&
		revisionSourcePlan == nil &&
		len(revisionCommentIDs) == 0 &&
		revisionSourceDiff == nil &&
		len(revisionDiffCommentIDs) == 0 {
		return "", nil
	}
	metaAttachments := make([]AttachmentMeta, 0, len(attachments))
	for _, attachment := range attachments {
		metaAttachments = append(metaAttachments, AttachmentMeta{
			ID:       attachment.ID,
			ThreadID: attachment.ThreadID,
			Filename: attachment.Filename,
			MimeType: attachment.MimeType,
			Size:     attachment.Size,
		})
	}
	meta := Meta{
		Attachments:                  metaAttachments,
		SourceProposedPlan:           sourcePlan,
		RevisionSourceProposedPlan:   revisionSourcePlan,
		RevisionSourceCommentIDs:     revisionCommentIDs,
		RevisionSourceDiffReview:     revisionSourceDiff,
		RevisionSourceDiffCommentIDs: revisionDiffCommentIDs,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// FromItem decodes the user_text Meta column back into a Meta. An
// empty / whitespace-only Meta returns the zero Meta with no error
// so callers can treat "row was written before meta existed" the
// same as "row deliberately has no meta".
func FromItem(item store.Item) (Meta, error) {
	var meta Meta
	if strings.TrimSpace(item.Meta) == "" {
		return meta, nil
	}
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		return Meta{}, fmt.Errorf("decode user message meta: %w", err)
	}
	return meta, nil
}

// EncodeDraftSource returns the JSON encoding of a source-proposed-plan
// ref suitable for ThreadDraft.PendingPlanImplementation. A nil ref or
// a ref with an empty ItemID returns ("", nil) so the draft stores SQL
// NULL — keeping the partial index introduced in store migration v31
// selective.
func EncodeDraftSource(ref *store.ProposedPlanSourceRef) (string, error) {
	if ref == nil || ref.ItemID == "" {
		return "", nil
	}
	b, err := json.Marshal(ref)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

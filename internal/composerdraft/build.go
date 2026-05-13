package composerdraft

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// FromUserItem composes a ThreadDraft row from a stored user_text item.
// The user item's `Summary` becomes the draft body; attachments
// referenced in the item's `Meta` (via usermessage.FromItem) are
// echoed back as a JSON array of attachment IDs; the source-plan
// linkage is preserved verbatim so the draft can re-link the
// implementation to its originating plan.
//
// Returns an empty TerminalChips array — terminal chips are
// composer-only context and not part of the persisted user item.
//
// Use this when the attachment IDs on the source item are valid for
// targetThreadID (e.g. the in-place revert-to-message path, where the
// thread doesn't change). Use the App-bound clone variant in
// `app_draft.go` when you need to clone attachment bytes across
// threads.
func FromUserItem(targetThreadID string, userItem store.Item, updatedAt int64) (store.ThreadDraft, error) {
	meta, err := usermessage.FromItem(userItem)
	if err != nil {
		return store.ThreadDraft{}, err
	}
	attachmentIDs := make([]string, 0, len(meta.Attachments))
	for _, attachment := range meta.Attachments {
		id := strings.TrimSpace(attachment.ID)
		if id != "" {
			attachmentIDs = append(attachmentIDs, id)
		}
	}
	return FromParts(targetThreadID, userItem.Summary, attachmentIDs, meta.SourceProposedPlan, updatedAt)
}

// FromParts assembles a ThreadDraft from a pre-resolved attachment-ID
// list and source-plan ref. Used by the cross-thread-clone variant in
// `app_draft.go` once the attachment IDs in targetThreadID's namespace
// have been resolved.
//
// Returns an empty TerminalChips array (matching FromUserItem) so the
// composer rehydrates with no inherited terminal context.
func FromParts(
	targetThreadID string,
	content string,
	attachmentIDs []string,
	sourceProposedPlan *store.ProposedPlanSourceRef,
	updatedAt int64,
) (store.ThreadDraft, error) {
	attachmentsJSON, err := json.Marshal(attachmentIDs)
	if err != nil {
		return store.ThreadDraft{}, fmt.Errorf("encode attachment ids: %w", err)
	}
	sourcePlanJSON, err := usermessage.EncodeDraftSource(sourceProposedPlan)
	if err != nil {
		return store.ThreadDraft{}, fmt.Errorf("encode source proposed plan: %w", err)
	}
	return store.ThreadDraft{
		ThreadID:                  targetThreadID,
		Content:                   content,
		Attachments:               string(attachmentsJSON),
		TerminalChips:             "[]",
		PendingPlanImplementation: sourcePlanJSON,
		UpdatedAt:                 updatedAt,
	}, nil
}

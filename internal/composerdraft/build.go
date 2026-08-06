package composerdraft

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// Part is one composer-draft fragment ready to merge into a draft row.
// AttachmentIDs are IDs only; callers that need to clone attachment bytes
// across threads must do that before constructing the part.
type Part struct {
	Content            string
	AttachmentIDs      []string
	SourceProposedPlan *store.ProposedPlanSourceRef
}

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
// targetThreadID (e.g. the Stop/Esc un-send, which rehydrates the
// message into the composer of the same thread). Use the App-bound
// clone variant in `app_draft.go` when you need to clone attachment
// bytes across threads.
func FromUserItem(targetThreadID string, userItem store.Item, updatedAt int64) (store.ThreadDraft, error) {
	part, err := PartFromUserItem(userItem)
	if err != nil {
		return store.ThreadDraft{}, err
	}
	return FromParts(targetThreadID, part.Content, part.AttachmentIDs, part.SourceProposedPlan, updatedAt)
}

// PartFromUserItem projects a stored user_text item into a draft part.
func PartFromUserItem(userItem store.Item) (Part, error) {
	meta, err := usermessage.FromItem(userItem)
	if err != nil {
		return Part{}, err
	}
	attachmentIDs := make([]string, 0, len(meta.Attachments))
	for _, attachment := range meta.Attachments {
		id := strings.TrimSpace(attachment.ID)
		if id != "" {
			attachmentIDs = append(attachmentIDs, id)
		}
	}
	return Part{
		Content:            userItem.Summary,
		AttachmentIDs:      attachmentIDs,
		SourceProposedPlan: meta.SourceProposedPlan,
	}, nil
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

// MergeParts merges restored draft parts into an existing thread draft.
// Restored messages come first in their caller-provided order, the
// existing draft content last, separated by blank lines — chronological
// order, matching the Codex TUI's composer restore (the restored
// messages were typed before whatever is sitting in the composer now).
// Attachment IDs are deduped while preserving first occurrence. If the
// existing draft already carries a pending plan implementation, it wins;
// otherwise a common source plan across restored parts is preserved.
func MergeParts(
	targetThreadID string,
	current store.ThreadDraft,
	parts []Part,
	updatedAt int64,
) (store.ThreadDraft, error) {
	contentParts := make([]string, 0, len(parts)+1)
	for _, part := range parts {
		if strings.TrimSpace(part.Content) != "" {
			contentParts = append(contentParts, strings.TrimSpace(part.Content))
		}
	}
	if strings.TrimSpace(current.Content) != "" {
		contentParts = append(contentParts, strings.TrimSpace(current.Content))
	}

	currentIDs, err := decodeAttachmentIDs(current.Attachments)
	if err != nil {
		return store.ThreadDraft{}, err
	}
	var attachmentIDs []string
	for _, part := range parts {
		attachmentIDs = appendUniqueStrings(attachmentIDs, part.AttachmentIDs)
	}
	attachmentIDs = appendUniqueStrings(attachmentIDs, currentIDs)
	attachmentsJSON, err := json.Marshal(attachmentIDs)
	if err != nil {
		return store.ThreadDraft{}, fmt.Errorf("encode attachment ids: %w", err)
	}

	pendingPlan := current.PendingPlanImplementation
	if pendingPlan == "" {
		pendingPlan, err = usermessage.EncodeDraftSource(commonSourcePlan(parts))
		if err != nil {
			return store.ThreadDraft{}, fmt.Errorf("encode source proposed plan: %w", err)
		}
	}

	terminalChips := current.TerminalChips
	if strings.TrimSpace(terminalChips) == "" {
		terminalChips = "[]"
	}

	return store.ThreadDraft{
		ThreadID:                  targetThreadID,
		Content:                   strings.Join(contentParts, "\n\n"),
		Attachments:               string(attachmentsJSON),
		TerminalChips:             terminalChips,
		PendingPlanImplementation: pendingPlan,
		UpdatedAt:                 updatedAt,
	}, nil
}

func decodeAttachmentIDs(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{}, nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, fmt.Errorf("decode existing draft attachments: %w", err)
	}
	return appendUniqueStrings(nil, ids), nil
}

func appendUniqueStrings(existing []string, additions []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(additions))
	out := make([]string, 0, len(existing)+len(additions))
	for _, value := range existing {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	for _, value := range additions {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func commonSourcePlan(parts []Part) *store.ProposedPlanSourceRef {
	var common *store.ProposedPlanSourceRef
	for _, part := range parts {
		sourcePlan := part.SourceProposedPlan
		if sourcePlan == nil || strings.TrimSpace(sourcePlan.ItemID) == "" {
			continue
		}
		if common == nil {
			copy := *sourcePlan
			common = &copy
			continue
		}
		if common.ThreadID != sourcePlan.ThreadID ||
			common.ItemID != sourcePlan.ItemID ||
			common.PayloadID != sourcePlan.PayloadID {
			return nil
		}
	}
	return common
}

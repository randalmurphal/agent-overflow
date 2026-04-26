package main

import (
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/store"
)

// TerminalChip is the frontend-owned shape of a "terminal context" snippet
// captured from the terminal drawer. The Go side treats these as opaque —
// we store and echo them back intact.
type TerminalChip struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Preview   string `json:"preview"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"createdAt"`
}

// Draft is the composer draft state. AttachmentIDs reference rows inserted
// by UploadAttachment; terminal chips are snippets captured from the
// terminal drawer. SourceProposedPlan, when non-nil, links the draft to a
// proposed plan in another thread — set by "Implement plan in new thread"
// so the eventual send carries the linkage that marks the original plan
// Accepted.
type Draft struct {
	ThreadID           string              `json:"threadId"`
	Content            string              `json:"content"`
	AttachmentIDs      []string            `json:"attachmentIds"`
	TerminalChips      []TerminalChip      `json:"terminalChips"`
	SourceProposedPlan *SourceProposedPlan `json:"sourceProposedPlan,omitempty"`
	UpdatedAt          int64               `json:"updatedAt"`
}

// SaveDraft replaces the draft row for a thread.
func (a *App) SaveDraft(threadID string, content string, attachmentIDs []string, terminalChips []TerminalChip, sourceProposedPlan *SourceProposedPlan) error {
	if a.store == nil {
		return fmt.Errorf("draft store not initialized")
	}
	attachmentsJSON, err := json.Marshal(normalizeAttachmentIDs(attachmentIDs))
	if err != nil {
		return fmt.Errorf("save draft: encode attachment ids: %w", err)
	}
	chipsJSON, err := json.Marshal(normalizeTerminalChips(terminalChips))
	if err != nil {
		return fmt.Errorf("save draft: encode terminal chips: %w", err)
	}
	sourcePlanJSON, err := encodeDraftSourceProposedPlan(sourceProposedPlan)
	if err != nil {
		return fmt.Errorf("save draft: encode source proposed plan: %w", err)
	}
	return a.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID:                  threadID,
		Content:                   content,
		Attachments:               string(attachmentsJSON),
		TerminalChips:             string(chipsJSON),
		PendingPlanImplementation: sourcePlanJSON,
		UpdatedAt:                 time.Now().UnixMilli(),
	})
}

// GetDraft returns the draft for a thread. Missing drafts return a zero
// Draft (with empty arrays) rather than an error.
func (a *App) GetDraft(threadID string) (Draft, error) {
	if a.store == nil {
		return Draft{}, fmt.Errorf("draft store not initialized")
	}
	row, _, err := a.store.GetThreadDraft(threadID)
	if err != nil {
		return Draft{}, err
	}

	draft := Draft{
		ThreadID:      row.ThreadID,
		Content:       row.Content,
		AttachmentIDs: []string{},
		TerminalChips: []TerminalChip{},
		UpdatedAt:     row.UpdatedAt,
	}
	if row.Attachments != "" {
		if err := json.Unmarshal([]byte(row.Attachments), &draft.AttachmentIDs); err != nil {
			return Draft{}, fmt.Errorf("get draft: decode attachment ids: %w", err)
		}
	}
	if draft.AttachmentIDs == nil {
		draft.AttachmentIDs = []string{}
	}
	if row.TerminalChips != "" {
		if err := json.Unmarshal([]byte(row.TerminalChips), &draft.TerminalChips); err != nil {
			return Draft{}, fmt.Errorf("get draft: decode terminal chips: %w", err)
		}
	}
	if draft.TerminalChips == nil {
		draft.TerminalChips = []TerminalChip{}
	}
	if row.PendingPlanImplementation != "" {
		var src SourceProposedPlan
		if err := json.Unmarshal([]byte(row.PendingPlanImplementation), &src); err != nil {
			return Draft{}, fmt.Errorf("get draft: decode source proposed plan: %w", err)
		}
		draft.SourceProposedPlan = &src
	}
	return draft, nil
}

// ClearDraft deletes any stored draft for a thread. Missing rows are not
// treated as an error because the caller just wants the thread to have no
// draft.
func (a *App) ClearDraft(threadID string) error {
	if a.store == nil {
		return fmt.Errorf("draft store not initialized")
	}
	return a.store.DeleteThreadDraft(threadID)
}

func normalizeAttachmentIDs(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

func normalizeTerminalChips(chips []TerminalChip) []TerminalChip {
	if chips == nil {
		return []TerminalChip{}
	}
	return chips
}

// encodeDraftSourceProposedPlan returns "" for a nil ref so UpsertThreadDraft
// stores SQL NULL (and the partial index in v31 stays selective). A
// zero-itemId ref is treated as nil — no item to link to.
func encodeDraftSourceProposedPlan(ref *SourceProposedPlan) (string, error) {
	if ref == nil || ref.ItemID == "" {
		return "", nil
	}
	b, err := json.Marshal(ref)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

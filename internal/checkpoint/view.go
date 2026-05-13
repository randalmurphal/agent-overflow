package checkpoint

import (
	"agent-overflow/internal/diffsummary"
	"agent-overflow/internal/store"
)

// View is the wire-side projection of a store.Checkpoint row. It
// drops the server-internal columns (RefName, BaselineSHA,
// WorkspacePath, ProviderParentUUID) that the frontend has no use
// for, and keeps the fields the timeline-restore UI renders.
//
// Kept here (not in main) so the projection is impossible to bypass:
// every wire-bound caller hits ViewFromStore and a future
// store.Checkpoint field that needs special handling has exactly one
// place to update.
type View struct {
	ID                    string             `json:"id"`
	ThreadID              string             `json:"threadId"`
	UserItemID            string             `json:"userItemId"`
	TurnIndex             int                `json:"turnIndex"`
	ProviderUserMessageID string             `json:"providerUserMessageId,omitempty"`
	Status                string             `json:"status"`
	Files                 []diffsummary.File `json:"files"`
	CapturedAt            int64              `json:"capturedAt"`
}

// ViewFromStore projects a store.Checkpoint row onto the wire shape.
func ViewFromStore(row store.Checkpoint) View {
	return View{
		ID:                    row.ID,
		ThreadID:              row.ThreadID,
		UserItemID:            row.UserItemID,
		TurnIndex:             row.TurnIndex,
		ProviderUserMessageID: row.ProviderUserMessageID,
		Status:                row.Status,
		Files:                 row.Files,
		CapturedAt:            row.CapturedAt,
	}
}

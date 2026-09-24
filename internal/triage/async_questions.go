package triage

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/userquestion"
)

func (r *Router) handleAsyncQuestions(evt provider.ProviderEvent) (bool, error) {
	meta, structured, err := userquestion.Decode(evt.Meta)
	if err != nil {
		return structured, fmt.Errorf("async questions: %w", err)
	}
	if !structured {
		return false, nil
	}
	if evt.ItemID == "" {
		return true, fmt.Errorf("async question message has no item identity")
	}
	turnIndex, err := r.turnIndexForEvent(evt)
	if err != nil {
		return true, err
	}
	meta.BlockType = ""
	meta.ProviderItemID = evt.ItemID
	raw, err := json.Marshal(meta)
	if err != nil {
		return true, err
	}
	now := eventTimestampMillis(evt)
	row := store.Item{
		ID: userquestion.ItemID(evt.ItemID), ThreadID: evt.ThreadID, TurnIndex: turnIndex,
		Kind: itemKindAssistantText, Role: "assistant", Status: statusCompleted, Summary: userquestion.Summary(meta.Questions),
		ParentID: eventParentID(evt), Meta: string(raw), CreatedAt: now, UpdatedAt: now,
	}
	var item store.Item
	err = r.withSubagentCard(row.ThreadID, row.ParentID, func(card *store.SubagentCard) error {
		row.SubagentCard = card
		var err error
		item, err = r.store.RecordAsyncQuestions(row)
		return err
	})
	if err != nil {
		return true, err
	}
	r.emit(eventchan.ProviderItemEvent, NewItemStreamUpsert(item))
	r.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": evt.ThreadID})
	return true, nil
}

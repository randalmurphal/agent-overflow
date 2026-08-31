package threadapp

import (
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// EnsureCanFork validates the store-backed preconditions shared by provider
// fork implementations.
func (s *Service) EnsureCanFork(source store.Thread, atTurnIndex *int) error {
	database, err := s.database("fork thread")
	if err != nil {
		return err
	}
	items, err := database.ListItems(source.ID)
	if err != nil {
		return fmt.Errorf("fork thread: list source items: %w", err)
	}
	if len(items) == 0 {
		return fmt.Errorf("fork thread: thread %q has no messages and cannot be forked", source.ID)
	}
	if atTurnIndex == nil {
		return nil
	}
	if *atTurnIndex < 0 {
		return fmt.Errorf("fork thread: atTurnIndex must be >= 0, got %d", *atTurnIndex)
	}
	lastTurn, err := database.LastTurnIndex(source.ID)
	if err != nil {
		return fmt.Errorf("fork thread: load source last turn index: %w", err)
	}
	if *atTurnIndex > lastTurn {
		return fmt.Errorf("fork thread: atTurnIndex %d exceeds source last turn %d", *atTurnIndex, lastTurn)
	}
	return nil
}

func (s *Service) SettleForkAsInterrupted(forkThreadID string) error {
	database, err := s.database("fork thread")
	if err != nil {
		return err
	}
	if err := database.SettleForkedThreadAsInterrupted(
		forkThreadID, triage.InterruptedSummary, s.deps.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("fork thread: settle fork as interrupted: %w", err)
	}
	return nil
}

// ResolveCodexForkAnchor picks the latest provider-backed turn at or before
// lastKeptTurnIndex. knownProviderTurns is injected because its store query is
// shared with rollback policy outside this package.
func (s *Service) ResolveCodexForkAnchor(
	threadID string,
	lastKeptTurnIndex int,
	knownProviderTurns func(threadID string, beforeTurnIndex int) (int, error),
) (string, bool, error) {
	database, err := s.database("resolve codex fork anchor")
	if err != nil {
		return "", false, err
	}
	for index := lastKeptTurnIndex; index >= 0; index-- {
		turn, found, err := database.GetTurnByThreadIndex(threadID, index)
		if err != nil {
			return "", false, fmt.Errorf("resolve codex fork anchor: %w", err)
		}
		if found && turn.ProviderTurnID != "" {
			return turn.ProviderTurnID, true, nil
		}
	}
	providerBacked, err := knownProviderTurns(threadID, lastKeptTurnIndex+1)
	if err != nil {
		return "", false, fmt.Errorf("resolve codex fork anchor: %w", err)
	}
	if providerBacked > 0 {
		return "", false, fmt.Errorf(
			"resolve codex fork anchor: thread %s has %d provider-backed turns at or before %d but no recorded provider turn id — likely a fork created before turn rows were cloned; fork the thread again from the desired message",
			threadID, providerBacked, lastKeptTurnIndex,
		)
	}
	return "", false, nil
}

// ComputeClaudeProviderIDRemap returns the store updates implied by uuidMap
// without applying them. The fork saga may apply them under its delete-on-
// failure cleanup, while rollback commits them atomically with SessionRef.
func (s *Service) ComputeClaudeProviderIDRemap(
	threadID string,
	uuidMap map[string]string,
) ([]store.ItemMetaUpdate, []store.MessageAnchorProviderIDsUpdate, error) {
	database, err := s.database("remap claude provider ids")
	if err != nil {
		return nil, nil, err
	}
	if len(uuidMap) == 0 {
		return nil, nil, nil
	}
	items, err := database.ListItems(threadID)
	if err != nil {
		return nil, nil, fmt.Errorf("remap claude provider ids: list items: %w", err)
	}
	var itemUpdates []store.ItemMetaUpdate
	for _, item := range items {
		if item.Kind != "user_text" || item.Role != "user" {
			continue
		}
		newUUID := uuidMap[usermessage.ReadProviderItemID(item.Meta)]
		newParent := uuidMap[usermessage.ReadProviderParentUUID(item.Meta)]
		if newUUID == "" && newParent == "" {
			continue
		}
		newMeta, err := usermessage.MergeProviderIDs(item.Meta, newUUID, newParent)
		if err != nil {
			return nil, nil, fmt.Errorf("remap claude provider ids: merge item %s/%s meta: %w", threadID, item.ID, err)
		}
		if newMeta != item.Meta {
			itemUpdates = append(itemUpdates, store.ItemMetaUpdate{ItemID: item.ID, Meta: newMeta})
		}
	}
	anchors, err := database.ListMessageAnchors(threadID)
	if err != nil {
		return nil, nil, fmt.Errorf("remap claude provider ids: list message anchors: %w", err)
	}
	var anchorUpdates []store.MessageAnchorProviderIDsUpdate
	for _, anchor := range anchors {
		newMessageID := uuidMap[anchor.ProviderUserMessageID]
		newParent := uuidMap[anchor.ProviderParentUUID]
		if newMessageID == "" && newParent == "" {
			continue
		}
		anchorUpdates = append(anchorUpdates, store.MessageAnchorProviderIDsUpdate{
			UserItemID:            anchor.UserItemID,
			ProviderUserMessageID: newMessageID,
			ProviderParentUUID:    newParent,
		})
	}
	return itemUpdates, anchorUpdates, nil
}

// ApplyClaudeProviderIDRemap applies a precomputed remap. Fork calls this only
// while its cleanup stack still owns the new row.
func (s *Service) ApplyClaudeProviderIDRemap(threadID string, uuidMap map[string]string) error {
	database, err := s.database("remap claude provider ids")
	if err != nil {
		return err
	}
	itemUpdates, anchorUpdates, err := s.ComputeClaudeProviderIDRemap(threadID, uuidMap)
	if err != nil {
		return err
	}
	for _, update := range itemUpdates {
		if err := database.UpdateItemMeta(threadID, update.ItemID, update.Meta); err != nil {
			return fmt.Errorf("remap claude provider ids: update item %s/%s meta: %w", threadID, update.ItemID, err)
		}
	}
	for _, update := range anchorUpdates {
		if err := database.UpdateMessageAnchorProviderIDs(
			threadID, update.UserItemID, update.ProviderUserMessageID, update.ProviderParentUUID,
		); err != nil {
			return fmt.Errorf("remap claude provider ids: update anchor %s/%s: %w", threadID, update.UserItemID, err)
		}
	}
	return nil
}

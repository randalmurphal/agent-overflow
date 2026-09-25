package threadapp

import (
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// EnsureCanFork validates the store-backed preconditions shared by provider
// fork implementations.
func (s *Service) EnsureCanFork(source store.Thread, atTurnIndex *int) error {
	database, err := s.database("fork thread")
	if err != nil {
		return err
	}
	hasItems, err := database.HasItems(source.ID)
	if err != nil {
		return fmt.Errorf("fork thread: list source items: %w", err)
	}
	if !hasItems {
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

// CreatePointerFork writes fork as a pointer fork of sourceID cut at cut.
// Rows the fork inherits while they are still running in the source are
// settled in the fork's own copy with the standard interrupted treatment,
// the same shapes as the boot crash sweep and a user interrupt; the source
// is not touched.
func (s *Service) CreatePointerFork(fork store.Thread, sourceID string, cut store.ForkCut) error {
	database, err := s.database("fork thread")
	if err != nil {
		return err
	}
	return database.CreatePointerFork(fork, sourceID, cut, triage.InterruptedSummary, s.deps.Now().UnixMilli())
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
			"resolve codex fork anchor: thread %s has %d provider-backed turns at or before %d but no recorded provider turn id — likely a fork made before forks carried turn rows; fork the thread again from the desired message",
			threadID, providerBacked, lastKeptTurnIndex,
		)
	}
	return "", false, nil
}

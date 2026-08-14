package store

import (
	"fmt"
	"strings"
)

// ScopedTurnID returns the durable turns.turn_id for one logical turn.
//
// Provider turn ids are only meaningful inside their provider thread. They
// are deliberately preserved verbatim in turns.provider_turn_id for provider
// RPCs, while the row identity is scoped here so two sessions can never
// collide in the store's global turn_id key space. Providers without a wire
// id use the per-thread turn index.
func ScopedTurnID(threadID, providerTurnID string, turnIndex int) string {
	threadID = strings.TrimSpace(threadID)
	if id := strings.TrimSpace(providerTurnID); id != "" {
		return threadID + ":" + id
	}
	return fmt.Sprintf("%s:%d", threadID, turnIndex)
}

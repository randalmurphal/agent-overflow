package app

import (
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

type userMessageIntent uint8

const (
	messageDirect         userMessageIntent = iota // Internal turn-opening send.
	messageComposer                                // Queue when the backend is active or starting.
	messageLegacyComposer                          // Caller predicts numeric IDs; only idle admission is safe.
	messageSteer                                   // Legacy Codex-only, active-only RPC.
	messageFlush                                   // Dispatch an accepted queue item.
	messageFlushFallback                           // Provider refused steer: start a fresh turn.
)

type userMessagePersistence uint8

const (
	messagePersist userMessagePersistence = iota
	messagePersistQuiet
	messageDeferUntilEcho
)

// Placement is a dispatch decision, not an execution state. Display position
// and the response's logical turn differ for Claude input consumed mid-turn.
// Provider confirmation remains responsible for the consumption boundary.
type userMessagePlacement struct {
	displayTurn  int
	responseTurn int
	persistence  userMessagePersistence
	queue        bool
}

// resolveUserMessagePlacement is the sole App-side owner of turn placement.
// Call under the thread action lock; provider events can still complete a turn
// concurrently, so a rejected steer must explicitly resolve a fresh fallback.
func (a *App) resolveUserMessagePlacement(thread store.Thread, intent userMessageIntent) (userMessagePlacement, error) {
	var active store.Turn
	var found bool
	composer := intent == messageComposer || intent == messageLegacyComposer
	if composer || intent == messageSteer || intent == messageFlush {
		var err error
		active, found, err = a.store.GetActiveTurn(thread.ID)
		if err != nil {
			return userMessagePlacement{}, fmt.Errorf("inspect active turn: %w", err)
		}
	}
	if composer && !found {
		var err error
		found, err = a.userMessageAwaitingStart(thread.ID)
		if err != nil {
			return userMessagePlacement{}, err
		}
	}
	if composer && found {
		if intent == messageLegacyComposer {
			return userMessagePlacement{}, fmt.Errorf("This thread is already working. Reopen the app to update it, or wait for the turn to finish before sending again.")
		}
		return userMessagePlacement{queue: true}, nil
	}
	if intent == messageSteer {
		if thread.Provider != string(provider.Codex) {
			return userMessagePlacement{}, fmt.Errorf("steer message: not supported for provider %q", thread.Provider)
		}
		if !found {
			return userMessagePlacement{}, codex.ErrNoActiveTurn
		}
		return userMessagePlacement{displayTurn: active.TurnIndex, responseTurn: active.TurnIndex}, nil
	}
	if intent == messageFlush && found && thread.Provider == string(provider.Codex) {
		return userMessagePlacement{displayTurn: active.TurnIndex, responseTurn: active.TurnIndex, persistence: messageDeferUntilEcho}, nil
	}
	next, err := a.nextSendTurnIndex(thread.ID)
	if err != nil {
		return userMessagePlacement{}, err
	}
	placement := userMessagePlacement{displayTurn: next, responseTurn: next}
	if intent == messageFlush || intent == messageFlushFallback {
		placement.persistence = messageDeferUntilEcho
		if intent == messageFlush && found {
			placement.displayTurn = active.TurnIndex
			placement.persistence = messagePersistQuiet
		}
	}
	return placement, nil
}

func (a *App) userMessageAwaitingStart(threadID string) (bool, error) {
	if a.triage == nil {
		return false, nil
	}
	index, pending := a.triage.MaxPendingSendTurnIndex(threadID)
	if !pending {
		return false, nil
	}
	turn, found, err := a.store.GetTurnByThreadIndex(threadID, index)
	if err != nil {
		return false, fmt.Errorf("inspect pending turn: %w", err)
	}
	// Correlation survives completion for late echoes. It is not activity.
	return !found || turn.CompletedAt == nil, nil
}

func (a *App) nextSendTurnIndex(threadID string) (int, error) {
	next, err := a.store.NextTurnIndex(threadID)
	if err != nil {
		return 0, err
	}
	if a.triage != nil {
		if pending, ok := a.triage.MaxPendingSendTurnIndex(threadID); ok && pending >= next {
			next = pending + 1
		}
	}
	return next, nil
}

// Client identities do not encode placement: a turn/steer → turn/start race
// changes coordinates, never the accepted message or provider correlation ID.
// Hash arbitrary client input to preserve the reserved ID grammar. Empty IDs
// retain internal allocation; legacy composer callers also need numeric IDs
// even when they supply an idempotency ID.
func (a *App) userMessageItemID(threadID, sendID string, turnIndex int, intent userMessageIntent) (string, error) {
	scope := ""
	switch intent {
	case messageSteer:
		scope = "steer"
	case messageFlush, messageFlushFallback:
		scope = "flush"
	}
	if sendID != "" && intent != messageLegacyComposer {
		id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(sendID)).String()
		if scope != "" {
			return "user:" + scope + ":" + id, nil
		}
		return "user:" + id, nil
	}
	if scope == "flush" {
		return a.nextFlushUserItemID(threadID, turnIndex)
	}
	if scope == "steer" {
		return a.nextSteerUserItemID(threadID, turnIndex)
	}
	return fmt.Sprintf("user:%d", turnIndex), nil
}

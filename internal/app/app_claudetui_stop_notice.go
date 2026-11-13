package app

import (
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// The interactive Claude TUI drops a just-sent prompt from the model's
// context when Esc reaches it before any reply, while AO's transcript keeps
// the message. After such a stop the thread gets a warning notice saying so.

const claudeTUIDiscardedMessageNotice = "Claude's terminal UI discards a message that is stopped before it replies, " +
	"so the agent likely did not keep your last message. Send it again if it still applies."

// claudeTUIPreReplyKinds are the rows a turn may hold besides the user's
// message while the TUI has not replied: the model's unfinished reasoning
// and request-level retries and errors. Any other kind is agent output, or
// content that entered the conversation, and the TUI keeps the turn.
var claudeTUIPreReplyKinds = map[provider.ItemKind]bool{
	provider.ItemThinking: true,
	provider.ItemAPIRetry: true,
	provider.ItemAPIError: true,
	provider.ItemError:    true,
}

// claudeTUIPendingSendTurn returns the turn of a claude-tui send still
// awaiting its echo, or -1. A send returns once its prompt is submitted and
// its turn opens on the TUI's first request, so a stop in between finds no
// open turn. The caller samples this before the open turn: the event worker
// opens the turn and consumes the pending send without the thread action
// lock, and in the other order a turn opening between the two reads would
// show neither.
func (a *App) claudeTUIPendingSendTurn(threadID string, sess session) int {
	if sess.Provider != string(provider.ClaudeTUI) || a.triage == nil {
		return -1
	}
	turn, ok := a.triage.MaxPendingSendTurnIndex(threadID)
	if !ok {
		return -1
	}
	return turn
}

// claudeTUIStopDiscardsMessage reports whether an Esc delivered now to sess
// makes the TUI discard the message in flight: sess is a claude-tui
// session, and the turn holds the user's message and only
// claudeTUIPreReplyKinds rows. openTurn is the open turn, or negative when
// none is open; the turn is then pendingSendTurn (claudeTUIPendingSendTurn),
// whose send is the message even while its row waits for the echo, as a
// queued message's does. The caller holds the thread action lock and
// samples before sending the Esc.
func (a *App) claudeTUIStopDiscardsMessage(threadID string, sess session, openTurn, pendingSendTurn int) bool {
	if sess.Provider != string(provider.ClaudeTUI) {
		return false
	}
	turnIndex, hasMessage := openTurn, false
	if turnIndex < 0 {
		if pendingSendTurn < 0 {
			return false
		}
		turnIndex, hasMessage = pendingSendTurn, true
	}
	items, err := a.store.ListTurnItems(threadID, turnIndex)
	if err != nil {
		log.Printf("app: thread %s: read stopped claude-tui turn %d: %v", threadID, turnIndex, err)
		return false
	}
	for _, item := range items {
		if isReaderAuthoredUserItem(item) {
			hasMessage = true
			continue
		}
		if !claudeTUIPreReplyKinds[provider.ItemKind(item.Kind)] {
			return false
		}
	}
	return hasMessage
}

// noticeClaudeTUIDiscardedMessage posts the notice after a stop that
// claudeTUIStopDiscardsMessage sampled as discarding the message.
func (a *App) noticeClaudeTUIDiscardedMessage(threadID string) {
	if err := a.emitNoticeToThread(threadID, noticeKindWarning, claudeTUIDiscardedMessageNotice); err != nil {
		log.Printf("app: thread %s: %v", threadID, err)
	}
}

// isReaderAuthoredUserItem reports whether item is a top-level message the
// user sent, as opposed to a subagent prompt (parented) or a wire-only echo
// of content the provider consumed without an AO send.
func isReaderAuthoredUserItem(item store.Item) bool {
	return item.Kind == string(provider.ItemUserText) &&
		item.Role == "user" &&
		item.ParentID == "" &&
		!store.IsWireOnlyUserItem(item)
}

package main

import (
	"fmt"
	"log"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/store"
)

// discussionTurnPromptLimit bounds how many unseen channel messages
// promptDiscussionSpeaker loads for a single turn prompt. Generous
// enough that a normal discussion (MaxTurns default 8, plus a handful
// of human interjections) never truncates, while still bounding the
// query against a channel that somehow grew large.
const discussionTurnPromptLimit = 500

// promptDiscussionSpeaker dispatches a turn prompt to speakerThreadID:
// it loads the channel messages that thread hasn't seen yet (everything
// after its own last post), renders them via discussion.BuildTurnPrompt,
// and sends the result into the participant's provider session exactly
// like a normal chat message. Synchronous and fully testable — callers
// on a hot path use promptDiscussionSpeakerAsync instead.
//
// The caller must already have claimed the turn via
// Deliberation.TryClaimCurrentSpeaker (see claimAndPromptNextSpeaker)
// before invoking this — it only ever un-claims (ClearAwaitingResponse
// on a failed dispatch), it never claims. Keeping the claim out of this
// function is what makes the claim atomic in the caller's goroutine
// instead of racing a concurrent trigger across the async hop into
// here; see TryClaimCurrentSpeaker's doc comment for the race it closes.
func (a *App) promptDiscussionSpeaker(channelID, speakerThreadID string) error {
	if a.store == nil || a.channels == nil {
		return fmt.Errorf("discussion services unavailable")
	}

	channel, err := a.store.GetChannel(channelID)
	if err != nil {
		return err
	}
	if channel.Status != "open" {
		return fmt.Errorf("channel %s is not open", channelID)
	}

	speaker, err := a.store.GetThread(speakerThreadID)
	if err != nil {
		return err
	}

	lastSeenSeq, err := a.store.LastChannelMessageSeqFrom(channelID, speakerThreadID)
	if err != nil {
		return err
	}
	messages, err := a.channels.GetMessages(channelID, lastSeenSeq, discussionTurnPromptLimit)
	if err != nil {
		return err
	}

	prompt := discussion.BuildTurnPrompt(discussion.RoleFromThreadTitle(speaker.Title), messages)
	if err := a.sendMessage(speakerThreadID, prompt, nil); err != nil {
		// Un-claim so a later trigger (another human post, another
		// participant's turn landing) can retry prompting this same
		// speaker instead of the deliberation being stuck awaiting a
		// reply that was never actually sent.
		if deliberation, dErr := a.deliberationForChannel(channelID); dErr == nil {
			deliberation.ClearAwaitingResponse()
			a.emitDiscussionState(channelID)
		}
		return fmt.Errorf("dispatch discussion turn prompt to %s: %w", speakerThreadID, err)
	}
	return nil
}

// promptDiscussionSpeakerAsync fires promptDiscussionSpeaker in the
// background so hot-path callers don't block on a provider dispatch. A
// failure is never silently dropped: it's logged AND surfaced as
// thread error state on the PARENT thread (channel.ThreadID) via
// emitWireErrorToThread, mirroring how sessionEventHandler reports a
// syncDiscussionTurn failure.
func (a *App) promptDiscussionSpeakerAsync(channelID, speakerThreadID string) {
	go func() {
		if err := a.promptDiscussionSpeaker(channelID, speakerThreadID); err != nil {
			log.Printf("discussion: prompt speaker %s for channel %s: %v", speakerThreadID, channelID, err)
			channel, getErr := a.store.GetChannel(channelID)
			if getErr != nil {
				log.Printf("discussion: resolve channel for prompt-failure emit %s: %v", channelID, getErr)
				return
			}
			a.emitWireErrorToThread(channel.ThreadID, fmt.Sprintf("discussion turn prompt failed: %v", err))
		}
	}()
}

// claimAndPromptNextSpeaker atomically claims deliberation's current
// speaker and, on success, emits the updated discussion:state and
// dispatches the turn prompt asynchronously. No-ops when the claim
// fails: either a turn is already in flight (AwaitingResponse), the
// deliberation concluded, or there is no current speaker (a
// single-participant roster never re-prompts itself).
//
// Shared by both turn-driving triggers — a human post
// (maybePromptNextDiscussionSpeaker) and a participant's completed
// turn (recordDiscussionPost) — so the claim-then-dispatch sequence
// has exactly one implementation to keep correct.
func (a *App) claimAndPromptNextSpeaker(channelID string, deliberation *discussion.Deliberation) {
	speaker, ok := deliberation.TryClaimCurrentSpeaker()
	if !ok {
		return
	}
	a.emitDiscussionState(channelID)
	a.promptDiscussionSpeakerAsync(channelID, speaker)
}

// deliberationForChannel resolves the live in-memory FSM for a channel,
// rebuilding it from SQLite when the process has restarted since the
// channel was opened (a.deliberations is populated at startDiscussion
// time and is never persisted itself — see root CLAUDE.md principle 3).
// Only "deliberation" channels that are still "open" are rebuildable; a
// concluded/closed channel has nothing left to coordinate, so a miss
// there is a real not-found rather than something to reconstruct.
func (a *App) deliberationForChannel(channelID string) (*discussion.Deliberation, error) {
	if d, ok := a.deliberation(channelID); ok {
		return d, nil
	}
	if a.store == nil {
		return nil, fmt.Errorf("discussion store unavailable")
	}

	channel, err := a.store.GetChannel(channelID)
	if err != nil {
		return nil, err
	}
	if channel.Type != "deliberation" || channel.Status != "open" {
		return nil, fmt.Errorf("no active deliberation for channel %s", channelID)
	}

	participants, err := a.discussionRoster(channel)
	if err != nil {
		return nil, err
	}
	turnCount, err := a.store.CountChannelMessagesByType(channelID, "agent")
	if err != nil {
		return nil, err
	}
	currentSpeaker, err := a.rebuildCurrentSpeaker(channelID, participants)
	if err != nil {
		return nil, err
	}

	rebuilt := discussion.RestoreDeliberation(channelID, channel.MaxTurns, participants, turnCount, currentSpeaker)

	a.mu.Lock()
	defer a.mu.Unlock()
	// Double-checked locking: another goroutine may have rebuilt (or a
	// live discussion may have been installed) between the map-miss
	// above and this lock. Keep whichever instance landed first so
	// turn-driving state never forks across two Deliberation objects
	// for the same channel.
	if existing, ok := a.deliberations[channelID]; ok {
		return existing, nil
	}
	if a.deliberations == nil {
		a.deliberations = make(map[string]*discussion.Deliberation)
	}
	a.deliberations[channelID] = rebuilt
	return rebuilt, nil
}

// discussionRoster returns a channel's participant thread IDs in
// round-robin (definition) order: the parent thread's child threads,
// filtered to the ones actually linked to this channel. ListChildThreads
// orders by (created_at, rowid) so the roster is deterministic even
// though every participant thread is created in the same millisecond.
func (a *App) discussionRoster(channel store.Channel) ([]string, error) {
	children, err := a.store.ListChildThreads(channel.ThreadID)
	if err != nil {
		return nil, err
	}
	participants := make([]string, 0, len(children))
	for _, child := range children {
		if child.DiscussionID == channel.ID {
			participants = append(participants, child.ID)
		}
	}
	return participants, nil
}

// rebuildCurrentSpeaker recomputes CurrentSpeaker for a restart-rebuilt
// deliberation: the participant after the last agent poster in
// round-robin order, or participants[0] when no agent has posted yet
// (mirrors NewDeliberation's initial-state rule). The channel's message
// history is scanned directly rather than adding another store query —
// deliberation channels are turn-limited (MaxTurns, default 8) plus a
// handful of human interjections, so this is a small bounded read even
// on a long-lived channel.
func (a *App) rebuildCurrentSpeaker(channelID string, participants []string) (string, error) {
	if len(participants) == 0 {
		return "", nil
	}
	messages, err := a.channels.GetMessages(channelID, -1, 0)
	if err != nil {
		return "", err
	}
	var lastAgentPoster string
	for _, msg := range messages {
		if msg.FromType == "agent" {
			lastAgentPoster = msg.FromID
		}
	}
	if lastAgentPoster == "" {
		return participants[0], nil
	}
	return discussion.NextSpeakerAfter(participants, lastAgentPoster), nil
}

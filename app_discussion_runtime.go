package main

import (
	"fmt"
	"strings"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/store"
)

func (a *App) syncDiscussionTurn(threadID string) error {
	if a.store == nil || a.channels == nil {
		return nil
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}
	if thread.ParentThreadID == "" || thread.DiscussionID == "" {
		return nil
	}

	channel, err := a.store.GetChannel(thread.DiscussionID)
	if err != nil {
		return err
	}
	if channel.Status != "open" {
		return nil
	}

	item, found, err := a.latestAssistantTurn(thread.ID)
	if err != nil {
		return err
	}
	// A tool-only turn (no assistant text) still counts as this
	// participant's turn. Skip the channel mirror — there's no text to
	// post — but still record the post below: without this, a
	// tool-only turn would silently leave the deliberation awaiting a
	// reply forever (the FSM never learns the turn happened).
	if found {
		msg, postErr := a.channels.PostMessage(discussion.PostMessageInput{
			ChannelID: thread.DiscussionID,
			FromType:  "agent",
			FromID:    thread.ID,
			FromRole:  discussion.RoleFromThreadTitle(thread.Title),
			Content:   item.Summary,
		})
		if postErr != nil {
			return postErr
		}
		a.emitDiscussionMessage(thread.DiscussionID, msg)
	}

	return a.recordDiscussionPost(thread.DiscussionID, thread.ID)
}

func (a *App) latestAssistantTurn(threadID string) (store.Item, bool, error) {
	turnIndex, err := a.store.LastTurnIndex(threadID)
	if err != nil {
		return store.Item{}, false, err
	}

	item, found, err := a.store.FindTurnItem(threadID, turnIndex, "assistant_text")
	if err != nil || !found {
		return item, found, err
	}
	if item.Role != "assistant" || strings.TrimSpace(item.Summary) == "" {
		return store.Item{}, false, nil
	}
	return item, true, nil
}

// recordDiscussionPost advances the deliberation FSM for the post that
// just landed, then either concludes the channel or hands off to the
// next speaker. discussion:state is emitted after every call — both
// the advance and the conclude branch change turn-facing state the
// frontend needs to reflect immediately.
//
// Resolves via deliberationForChannel (not the plain in-memory
// a.deliberation lookup) so a participant's turn still advances the
// FSM after a process restart — e.g. a provider session that was
// mid-turn when the app restarted and only now emits its
// turn-complete. syncDiscussionTurn has already confirmed the channel
// is "open" before calling this, so a resolution failure here is a
// genuine store error, not the ordinary "no deliberation for this
// channel" case — it propagates like any other syncDiscussionTurn
// error (logged and surfaced via emitWireErrorToThread).
func (a *App) recordDiscussionPost(channelID, participantThreadID string) error {
	deliberation, err := a.deliberationForChannel(channelID)
	if err != nil {
		return err
	}

	_, shouldConclude := deliberation.RecordPost(participantThreadID)
	if shouldConclude {
		if err := a.concludeDiscussionChannel(channelID, deliberation.State().MaxTurns); err != nil {
			return err
		}
		a.emitDiscussionState(channelID)
		return nil
	}

	a.emitDiscussionState(channelID)
	a.claimAndPromptNextSpeaker(channelID, deliberation)
	return nil
}

// concludeDiscussionChannel posts the conclusion system message BEFORE
// flipping the channel status — ChannelService.PostMessage requires
// the channel still be "open", so the ordering here is load-bearing —
// then marks the channel concluded.
func (a *App) concludeDiscussionChannel(channelID string, maxTurns int) error {
	msg, err := a.channels.PostMessage(discussion.PostMessageInput{
		ChannelID: channelID,
		FromType:  "system",
		FromID:    "deliberation",
		FromRole:  "moderator",
		Content:   fmt.Sprintf("Discussion concluded: reached the %d-turn limit.", maxTurns),
	})
	if err != nil {
		return fmt.Errorf("post discussion conclusion message %s: %w", channelID, err)
	}
	a.emitDiscussionMessage(channelID, msg)

	if err := a.store.UpdateChannelStatus(channelID, "concluded"); err != nil {
		return fmt.Errorf("conclude discussion channel %s: %w", channelID, err)
	}
	return nil
}

func (a *App) deliberation(channelID string) (*discussion.Deliberation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	deliberation, ok := a.deliberations[channelID]
	return deliberation, ok
}

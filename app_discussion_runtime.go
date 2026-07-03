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

	// Resolve the deliberation BEFORE mirroring the post into the
	// channel — the ordering is load-bearing. On a cold a.deliberations
	// map (process restart) deliberationForChannel reconstructs
	// TurnCount from CountChannelMessagesByType(FromTypeAgent); if this
	// turn's row were already committed, the rebuilt count would
	// include it and RecordPost would then increment it AGAIN — N prior
	// turns becoming N+2 instead of N+1, concluding the discussion a
	// turn early. The channel is confirmed "open" above, so a
	// resolution failure here is a genuine store error — it propagates
	// like any other syncDiscussionTurn error (logged and surfaced via
	// emitWireErrorToThread).
	deliberation, err := a.deliberationForChannel(thread.DiscussionID)
	if err != nil {
		return err
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
			FromType:  discussion.FromTypeAgent,
			FromID:    thread.ID,
			FromRole:  discussion.RoleFromThreadTitle(thread.Title),
			Content:   item.Summary,
		})
		if postErr != nil {
			return postErr
		}
		a.emitDiscussionMessage(thread.DiscussionID, msg)

		// The posted channel content keeps the CONCLUDE line verbatim
		// (transcript honesty — the other participants must see the
		// stance in their own turn prompts); only the FSM's bookkeeping
		// reacts to it. A latest-stance proposal here always overrides
		// whatever this participant proposed earlier: no marker on this
		// turn rescinds a prior proposal exactly like a fresh post from
		// a participant who never proposed.
		if summary, ok := discussion.ParseConclusionProposal(item.Summary); ok {
			deliberation.ProposeConclusionFrom(thread.ID, summary)
		} else {
			deliberation.WithdrawConclusionProposal(thread.ID)
		}
	}

	return a.recordDiscussionPost(thread.DiscussionID, thread.ID, deliberation)
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
// next speaker. Exactly one discussion:state lands per call: the
// conclude branch emits after the channel row flips; the advance
// branch delegates the emission to claimAndPromptNextSpeaker when the
// claim succeeds (its post-claim snapshot carries the new
// CurrentSpeaker + AwaitingResponse and would supersede an earlier one
// anyway) and emits directly only when no claim happened.
//
// The deliberation is resolved by the caller (syncDiscussionTurn) via
// deliberationForChannel BEFORE the agent's channel row is committed —
// see the ordering comment there for why resolving after the post
// would double-count the triggering turn on a restart rebuild.
func (a *App) recordDiscussionPost(channelID, participantThreadID string, deliberation *discussion.Deliberation) error {
	_, shouldConclude := deliberation.RecordPost(participantThreadID)
	if shouldConclude {
		if err := a.concludeDiscussionChannel(channelID, deliberation); err != nil {
			return err
		}
		// A concluded channel has nothing left to coordinate — drop the
		// FSM so a.deliberations doesn't retain it until thread
		// deletion. Removal must precede the emission: buildChannelState
		// then serves the concluded snapshot from its SQLite fallback
		// branch (deliberationForChannel refuses non-open channels).
		a.removeDeliberationByID(channelID)
		a.emitDiscussionState(channelID)
		return nil
	}

	if claimed := a.claimAndPromptNextSpeaker(channelID, deliberation); !claimed {
		a.emitDiscussionState(channelID)
	}
	return nil
}

// concludeDiscussionChannel posts the conclusion system message BEFORE
// flipping the channel status — ChannelService.PostMessage requires
// the channel still be "open", so the ordering here is load-bearing —
// then marks the channel concluded.
//
// The message content is cause-aware: it's unanimous only when every
// roster participant has a live ConclusionProposals entry (>= 2
// participants — a single-participant "discussion" can't reach
// consensus with itself). Everything else, including the ordinary
// MaxTurns circuit breaker, renders the turn-limit form. This is
// derived structurally from the FSM's own state rather than threaded
// through as a separate "cause" flag, so a caller can never pass a
// cause that disagrees with what actually happened.
//
// Resolving the roster (for the unanimous form's per-role summary
// lines) is a cold path — conclusion happens once per discussion — so
// the extra GetChannel + discussionRoster query here is fine. A roster
// resolution failure is returned rather than silently degraded to the
// turn-limit message: presenting the wrong conclusion cause would be
// worse than failing loudly.
func (a *App) concludeDiscussionChannel(channelID string, deliberation *discussion.Deliberation) error {
	state := deliberation.State()
	participants := deliberation.Participants()

	unanimous := len(participants) >= 2
	for _, participant := range participants {
		if _, ok := state.ConclusionProposals[participant]; !ok {
			unanimous = false
			break
		}
	}

	channel, err := a.store.GetChannel(channelID)
	if err != nil {
		return fmt.Errorf("resolve channel for conclusion message %s: %w", channelID, err)
	}
	roster, err := a.discussionRoster(channel)
	if err != nil {
		return fmt.Errorf("resolve roster for conclusion message %s: %w", channelID, err)
	}
	roleByThreadID := make(map[string]string, len(roster))
	for _, child := range roster {
		roleByThreadID[child.ID] = discussion.RoleFromThreadTitle(child.Title)
	}

	content := discussion.BuildConclusionMessage(discussion.ConclusionMessageInput{
		Unanimous:           unanimous,
		MaxTurns:            state.MaxTurns,
		ParticipantsInOrder: participants,
		Proposals:           state.ConclusionProposals,
		RoleByThreadID:      roleByThreadID,
	})

	msg, err := a.channels.PostMessage(discussion.PostMessageInput{
		ChannelID: channelID,
		FromType:  discussion.FromTypeSystem,
		FromID:    "deliberation",
		FromRole:  "moderator",
		Content:   content,
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

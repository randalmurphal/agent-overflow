package discussionapp

import (
	"fmt"
	"log"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/store"
)

func (s *Service) Post(channelID, content string) (store.ChannelMessage, error) {
	st, _, channels, err := s.services()
	if err != nil {
		return store.ChannelMessage{}, fmt.Errorf("channel service unavailable")
	}
	message, err := channels.PostMessage(discussion.PostMessageInput{
		ChannelID: channelID, FromType: discussion.FromTypeHuman,
		FromID: "user", FromRole: "human", Content: content,
	})
	if err != nil {
		return store.ChannelMessage{}, err
	}
	s.emitMessage(channelID, message)
	s.maybePromptNext(st, channelID)
	return message, nil
}

func (s *Service) maybePromptNext(st *store.Store, channelID string) {
	channel, err := st.GetChannel(channelID)
	if err != nil {
		log.Printf("discussion: resolve channel for next-speaker prompt %s: %v", channelID, err)
		return
	}
	if channel.Status != "open" {
		return
	}
	deliberation, err := s.deliberationForChannel(channelID)
	if err != nil {
		log.Printf("discussion: resolve deliberation for next-speaker prompt %s: %v", channelID, err)
		return
	}
	if state := deliberation.State(); state.Concluded {
		if concludeErr := s.concludeChannel(channelID, deliberation); concludeErr != nil {
			log.Printf("discussion: re-conclude wedged channel %s: %v", channelID, concludeErr)
		} else {
			s.Remove(channelID)
		}
		s.emitState(channelID)
		return
	}
	s.claimAndPrompt(channelID, deliberation)
}

func (s *Service) Conclude(channelID string) (State, error) {
	st, _, _, err := s.services()
	if err != nil {
		return State{}, err
	}
	channel, err := st.GetChannel(channelID)
	if err != nil {
		return State{}, err
	}
	if channel.Status != "open" {
		return State{}, fmt.Errorf("channel %s discussion already concluded", channelID)
	}
	content := discussion.BuildConclusionMessage(discussion.ConclusionMessageInput{Cause: discussion.ConclusionModerator})
	if err := s.postConclusion(channelID, content); err != nil {
		return State{}, err
	}
	s.Remove(channelID)
	s.emitState(channelID)
	return s.ChannelState(channelID)
}

func (s *Service) ChannelState(channelID string) (State, error) {
	st, _, _, err := s.services()
	if err != nil {
		return State{}, fmt.Errorf("discussion store unavailable")
	}
	channel, err := st.GetChannel(channelID)
	if err != nil {
		return State{}, err
	}
	result := State{ChannelID: channel.ID, ThreadID: channel.ThreadID, Status: channel.Status, MaxTurns: channel.MaxTurns}
	roster, err := discussionRoster(st, channel)
	if err != nil {
		return State{}, err
	}
	participantRows := roster
	var proposals map[string]string
	if deliberation, resolveErr := s.deliberationForChannel(channelID); resolveErr == nil {
		state := deliberation.State()
		result.TurnCount = state.TurnCount
		result.MaxTurns = state.MaxTurns
		result.AwaitingResponse = state.AwaitingResponse
		result.CurrentSpeakerThreadID = state.CurrentSpeaker
		proposals = state.ConclusionProposals
		rowsByID := make(map[string]store.Thread, len(roster))
		for _, child := range roster {
			rowsByID[child.ID] = child
		}
		participantRows = make([]store.Thread, 0, len(roster))
		for _, threadID := range deliberation.Participants() {
			if child, ok := rowsByID[threadID]; ok {
				participantRows = append(participantRows, child)
			}
		}
	} else {
		result.TurnCount, err = st.CountChannelMessagesByType(channelID, discussion.FromTypeAgent)
		if err != nil {
			return State{}, err
		}
	}
	result.Participants = make([]ParticipantState, 0, len(participantRows))
	for _, child := range participantRows {
		role := discussion.RoleFromThreadTitle(child.Title)
		_, proposed := proposals[child.ID]
		result.Participants = append(result.Participants, ParticipantState{
			ThreadID: child.ID, Role: role, Provider: child.Provider, Model: child.Model, ProposedConclusion: proposed,
		})
		if child.ID == result.CurrentSpeakerThreadID {
			result.CurrentSpeakerRole = role
		}
	}
	return result, nil
}

func (s *Service) emitMessage(channelID string, message store.ChannelMessage) {
	if s.events == nil {
		return
	}
	st, _, _, err := s.services()
	if err != nil {
		return
	}
	channel, err := st.GetChannel(channelID)
	if err != nil {
		log.Printf("discussion: resolve channel for message emit %s: %v", channelID, err)
		return
	}
	s.events.Message(MessageEvent{ChannelID: channel.ID, ThreadID: channel.ThreadID, Message: message})
}

func (s *Service) emitState(channelID string) {
	if s.events == nil {
		return
	}
	state, err := s.ChannelState(channelID)
	if err != nil {
		log.Printf("discussion: build state for emit %s: %v", channelID, err)
		return
	}
	s.events.State(state)
}

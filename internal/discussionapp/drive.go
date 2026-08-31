package discussionapp

import (
	"fmt"
	"log"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/store"
)

const turnPromptLimit = 500

func (s *Service) promptSpeaker(channelID, speakerThreadID string) error {
	st, _, channels, err := s.services()
	if err != nil {
		return err
	}
	channel, err := st.GetChannel(channelID)
	if err != nil {
		return err
	}
	if channel.Status != "open" {
		return fmt.Errorf("channel %s is not open", channelID)
	}
	speaker, err := st.GetThread(speakerThreadID)
	if err != nil {
		return err
	}
	lastSeenSeq, err := st.LastChannelMessageSeqFrom(channelID, speakerThreadID)
	if err != nil {
		return err
	}
	messages, err := channels.GetMessages(channelID, lastSeenSeq, turnPromptLimit)
	if err != nil {
		return err
	}
	prompt := discussion.BuildTurnPrompt(discussion.RoleFromThreadTitle(speaker.Title), messages)
	if s.runtime == nil {
		return fmt.Errorf("discussion participant runtime unavailable")
	}
	if err := s.runtime.SendParticipantMessage(speakerThreadID, prompt); err != nil {
		if deliberation, resolveErr := s.deliberationForChannel(channelID); resolveErr == nil {
			deliberation.ClearAwaitingResponse()
			s.emitState(channelID)
		} else {
			log.Printf("discussion: resolve deliberation to un-claim after failed dispatch %s: %v", channelID, resolveErr)
		}
		return fmt.Errorf("dispatch discussion turn prompt to %s: %w", speakerThreadID, err)
	}
	return nil
}

func (s *Service) promptSpeakerAsync(channelID, speakerThreadID string) {
	go func() {
		if err := s.promptSpeaker(channelID, speakerThreadID); err != nil {
			log.Printf("discussion: prompt speaker %s for channel %s: %v", speakerThreadID, channelID, err)
			st, _, _, serviceErr := s.services()
			if serviceErr != nil {
				return
			}
			channel, getErr := st.GetChannel(channelID)
			if getErr != nil {
				log.Printf("discussion: resolve channel for prompt-failure emit %s: %v", channelID, getErr)
				return
			}
			if s.events != nil {
				s.events.Error(channel.ThreadID, fmt.Sprintf("discussion turn prompt failed: %v", err))
			}
		}
	}()
}

func (s *Service) claimAndPrompt(channelID string, deliberation *discussion.Deliberation) bool {
	speaker, ok := deliberation.TryClaimCurrentSpeaker()
	if !ok {
		return false
	}
	s.emitState(channelID)
	s.promptSpeakerAsync(channelID, speaker)
	return true
}

func (s *Service) deliberation(channelID string) (*discussion.Deliberation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.deliberations[channelID]
	return d, ok
}

func (s *Service) deliberationForChannel(channelID string) (*discussion.Deliberation, error) {
	if d, ok := s.deliberation(channelID); ok {
		return d, nil
	}
	st, _, channels, err := s.services()
	if err != nil {
		return nil, fmt.Errorf("discussion store unavailable")
	}
	channel, err := st.GetChannel(channelID)
	if err != nil {
		return nil, err
	}
	if channel.Type != "deliberation" || channel.Status != "open" {
		return nil, fmt.Errorf("no active deliberation for channel %s", channelID)
	}
	roster, err := discussionRoster(st, channel)
	if err != nil {
		return nil, err
	}
	participants := make([]string, 0, len(roster))
	for _, child := range roster {
		participants = append(participants, child.ID)
	}
	turnCount, err := st.CountChannelMessagesByType(channelID, discussion.FromTypeAgent)
	if err != nil {
		return nil, err
	}
	lastAgentPoster, latestContentByPoster, err := scanAgentHistory(channels, channelID)
	if err != nil {
		return nil, err
	}
	rebuilt := discussion.RestoreDeliberation(
		channelID,
		channel.MaxTurns,
		participants,
		turnCount,
		rebuildCurrentSpeaker(participants, lastAgentPoster),
	)
	for _, participantID := range participants {
		content, ok := latestContentByPoster[participantID]
		if !ok {
			continue
		}
		if summary, proposed := discussion.ParseConclusionProposal(content); proposed {
			rebuilt.ProposeConclusionFrom(participantID, summary)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.deliberations[channelID]; ok {
		return existing, nil
	}
	s.deliberations[channelID] = rebuilt
	return rebuilt, nil
}

func discussionRoster(st *store.Store, channel store.Channel) ([]store.Thread, error) {
	children, err := st.ListChildThreads(channel.ThreadID)
	if err != nil {
		return nil, err
	}
	participants := make([]store.Thread, 0, len(children))
	for _, child := range children {
		if child.DiscussionID == channel.ID {
			participants = append(participants, child)
		}
	}
	return participants, nil
}

func scanAgentHistory(channels *discussion.ChannelService, channelID string) (string, map[string]string, error) {
	messages, err := channels.GetMessages(channelID, -1, 0)
	if err != nil {
		return "", nil, err
	}
	latest := make(map[string]string)
	last := ""
	for _, message := range messages {
		if message.FromType == discussion.FromTypeAgent {
			last = message.FromID
			latest[message.FromID] = message.Content
		}
	}
	return last, latest, nil
}

func rebuildCurrentSpeaker(participants []string, lastAgentPoster string) string {
	if len(participants) == 0 {
		return ""
	}
	if lastAgentPoster == "" {
		return participants[0]
	}
	return discussion.NextSpeakerAfter(participants, lastAgentPoster)
}

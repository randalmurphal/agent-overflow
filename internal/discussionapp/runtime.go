package discussionapp

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/store"
)

// SyncTurn mirrors a completed participant turn and advances its deliberation.
func (s *Service) SyncTurn(threadID string) error {
	st, _, channels, err := s.services()
	if err != nil {
		return nil
	}
	thread, err := st.GetThread(threadID)
	if err != nil {
		return err
	}
	if thread.ParentThreadID == "" || thread.DiscussionID == "" {
		return nil
	}
	channel, err := st.GetChannel(thread.DiscussionID)
	if err != nil {
		return err
	}
	if channel.Status != "open" {
		return nil
	}

	// Resolve before posting. A restart rebuild counts persisted agent rows;
	// reversing this order would count the triggering turn twice.
	deliberation, err := s.deliberationForChannel(thread.DiscussionID)
	if err != nil {
		return err
	}
	item, found, err := latestAssistantTurn(st, thread.ID)
	if err != nil {
		return err
	}
	if found {
		message, postErr := channels.PostMessage(discussion.PostMessageInput{
			ChannelID: thread.DiscussionID,
			FromType:  discussion.FromTypeAgent,
			FromID:    thread.ID,
			FromRole:  discussion.RoleFromThreadTitle(thread.Title),
			Content:   item.Summary,
		})
		if postErr != nil {
			if errors.Is(postErr, discussion.ErrChannelNotOpen) {
				log.Printf("discussion: channel %s concluded while turn in flight, dropping mirror for %s", thread.DiscussionID, thread.ID)
				return nil
			}
			return postErr
		}
		s.emitMessage(thread.DiscussionID, message)
		if summary, proposed := discussion.ParseConclusionProposal(item.Summary); proposed {
			deliberation.ProposeConclusionFrom(thread.ID, summary)
		} else {
			deliberation.WithdrawConclusionProposal(thread.ID)
		}
	}
	return s.recordPost(thread.DiscussionID, thread.ID, deliberation)
}

func latestAssistantTurn(st *store.Store, threadID string) (store.Item, bool, error) {
	turnIndex, err := st.LastTurnIndex(threadID)
	if err != nil {
		return store.Item{}, false, err
	}
	item, found, err := st.FindTurnItem(threadID, turnIndex, "assistant_text")
	if err != nil || !found {
		return item, found, err
	}
	if item.Role != "assistant" || strings.TrimSpace(item.Summary) == "" {
		return store.Item{}, false, nil
	}
	return item, true, nil
}

func (s *Service) recordPost(channelID, participantThreadID string, deliberation *discussion.Deliberation) error {
	_, shouldConclude := deliberation.RecordPost(participantThreadID)
	if shouldConclude {
		if err := s.concludeChannel(channelID, deliberation); err != nil {
			return err
		}
		s.Remove(channelID)
		s.emitState(channelID)
		return nil
	}
	if !s.claimAndPrompt(channelID, deliberation) {
		s.emitState(channelID)
	}
	return nil
}

func (s *Service) concludeChannel(channelID string, deliberation *discussion.Deliberation) error {
	st, _, _, err := s.services()
	if err != nil {
		return err
	}
	state := deliberation.State()
	participants := deliberation.Participants()
	cause := discussion.ConclusionTurnLimit
	unanimous := len(participants) >= 2
	for _, participant := range participants {
		if _, ok := state.ConclusionProposals[participant]; !ok {
			unanimous = false
			break
		}
	}
	if unanimous {
		cause = discussion.ConclusionUnanimous
	}
	channel, err := st.GetChannel(channelID)
	if err != nil {
		return fmt.Errorf("resolve channel for conclusion message %s: %w", channelID, err)
	}
	roster, err := discussionRoster(st, channel)
	if err != nil {
		return fmt.Errorf("resolve roster for conclusion message %s: %w", channelID, err)
	}
	roles := make(map[string]string, len(roster))
	for _, child := range roster {
		roles[child.ID] = discussion.RoleFromThreadTitle(child.Title)
	}
	content := discussion.BuildConclusionMessage(discussion.ConclusionMessageInput{
		Cause: cause, MaxTurns: state.MaxTurns, ParticipantsInOrder: participants,
		Proposals: state.ConclusionProposals, RoleByThreadID: roles,
	})
	return s.postConclusion(channelID, content)
}

func (s *Service) postConclusion(channelID, content string) error {
	st, _, channels, err := s.services()
	if err != nil {
		return err
	}
	message, err := channels.PostMessage(discussion.PostMessageInput{
		ChannelID: channelID, FromType: discussion.FromTypeSystem,
		FromID: "deliberation", FromRole: "moderator", Content: content,
	})
	if err != nil {
		return fmt.Errorf("post discussion conclusion message %s: %w", channelID, err)
	}
	// Message precedes the status flip by contract: PostMessage only accepts
	// open channels, and subscribers must observe the conclusion text first.
	s.emitMessage(channelID, message)
	if err := st.UpdateChannelStatus(channelID, "concluded"); err != nil {
		return fmt.Errorf("conclude discussion channel %s: %w", channelID, err)
	}
	return nil
}

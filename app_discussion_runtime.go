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
	if !found {
		return nil
	}

	if _, err := a.channels.PostMessage(discussion.PostMessageInput{
		ChannelID: thread.DiscussionID,
		FromType:  "agent",
		FromID:    thread.ID,
		FromRole:  discussionRoleFromThread(thread),
		Content:   item.Summary,
	}); err != nil {
		return err
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

func (a *App) recordDiscussionPost(channelID, participantThreadID string) error {
	deliberation, ok := a.deliberation(channelID)
	if !ok {
		return nil
	}

	_, shouldConclude := deliberation.RecordPost(participantThreadID)
	if !shouldConclude {
		return nil
	}
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

func discussionRoleFromThread(thread store.Thread) string {
	title := strings.TrimSpace(thread.Title)
	if title == "" {
		return ""
	}

	idx := strings.LastIndex(title, " - ")
	if idx < 0 {
		return title
	}
	role := strings.TrimSpace(title[idx+3:])
	if role == "" {
		return title
	}
	return role
}

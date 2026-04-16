package discussion

import (
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// ChannelService manages discussion channels and ordered channel messages.
type ChannelService struct {
	store *store.Store
}

// PostMessageInput describes a new channel message.
type PostMessageInput struct {
	ChannelID string `json:"channelId"`
	FromType  string `json:"fromType"`
	FromID    string `json:"fromId"`
	FromRole  string `json:"fromRole,omitempty"`
	Content   string `json:"content"`
}

// NewChannelService constructs a channel service.
func NewChannelService(st *store.Store) *ChannelService {
	return &ChannelService{store: st}
}

// Create opens a new channel for the given thread.
func (cs *ChannelService) Create(threadID, channelType string) (store.Channel, error) {
	if cs.store == nil {
		return store.Channel{}, fmt.Errorf("channel service unavailable")
	}

	threadID = strings.TrimSpace(threadID)
	channelType = strings.TrimSpace(channelType)
	if threadID == "" {
		return store.Channel{}, fmt.Errorf("thread ID is required")
	}
	if channelType == "" {
		channelType = "deliberation"
	}

	now := time.Now().UnixMilli()
	channel := store.Channel{
		ID:        uuid.New().String(),
		ThreadID:  threadID,
		Type:      channelType,
		Status:    "open",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := cs.store.CreateChannel(channel); err != nil {
		return store.Channel{}, err
	}
	return channel, nil
}

// PostMessage appends a new ordered message to the channel.
func (cs *ChannelService) PostMessage(input PostMessageInput) (store.ChannelMessage, error) {
	if cs.store == nil {
		return store.ChannelMessage{}, fmt.Errorf("channel service unavailable")
	}

	input.ChannelID = strings.TrimSpace(input.ChannelID)
	input.FromType = strings.TrimSpace(input.FromType)
	input.FromID = strings.TrimSpace(input.FromID)
	input.FromRole = strings.TrimSpace(input.FromRole)
	input.Content = strings.TrimSpace(input.Content)

	if input.ChannelID == "" {
		return store.ChannelMessage{}, fmt.Errorf("channel ID is required")
	}
	if input.FromType == "" {
		return store.ChannelMessage{}, fmt.Errorf("from type is required")
	}
	if input.FromID == "" {
		return store.ChannelMessage{}, fmt.Errorf("from ID is required")
	}
	if input.Content == "" {
		return store.ChannelMessage{}, fmt.Errorf("content is required")
	}

	channel, err := cs.store.GetChannel(input.ChannelID)
	if err != nil {
		return store.ChannelMessage{}, err
	}
	if channel.Status != "open" {
		return store.ChannelMessage{}, fmt.Errorf("channel %s is not open", input.ChannelID)
	}

	msg := store.ChannelMessage{
		ID:        uuid.New().String(),
		ChannelID: input.ChannelID,
		FromType:  input.FromType,
		FromID:    input.FromID,
		FromRole:  input.FromRole,
		Content:   input.Content,
		CreatedAt: time.Now().UnixMilli(),
	}
	sequence, err := cs.store.InsertChannelMessageAtomic(msg)
	if err != nil {
		return store.ChannelMessage{}, err
	}
	msg.Sequence = sequence
	return msg, nil
}

// GetMessages returns ordered channel messages after the given sequence cursor.
func (cs *ChannelService) GetMessages(channelID string, afterSeq, limit int) ([]store.ChannelMessage, error) {
	if cs.store == nil {
		return nil, fmt.Errorf("channel service unavailable")
	}
	return cs.store.ListChannelMessages(strings.TrimSpace(channelID), afterSeq, limit)
}

// Close marks a channel as closed.
func (cs *ChannelService) Close(channelID string) error {
	if cs.store == nil {
		return fmt.Errorf("channel service unavailable")
	}
	return cs.store.UpdateChannelStatus(strings.TrimSpace(channelID), "closed")
}


package discussion

import (
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/pathlinks"
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

func NewChannelService(st *store.Store) *ChannelService {
	return &ChannelService{store: st}
}

// Create opens a new channel for the given thread. maxTurns <= 0
// normalizes to DefaultMaxTurns — store.CreateChannel persists whatever
// it's handed verbatim, so the fallback has to happen here rather than
// in the store (package store must not import package discussion).
func (cs *ChannelService) Create(threadID, channelType string, maxTurns int) (store.Channel, error) {
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
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}

	now := time.Now().UnixMilli()
	channel := store.Channel{
		ID:        uuid.New().String(),
		ThreadID:  threadID,
		Type:      channelType,
		Status:    "open",
		MaxTurns:  maxTurns,
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
		Meta:      cs.buildChannelMessageMeta(channel.ThreadID, input.Content),
		CreatedAt: time.Now().UnixMilli(),
	}
	sequence, err := cs.store.InsertChannelMessageAtomic(msg)
	if err != nil {
		return store.ChannelMessage{}, err
	}
	msg.Sequence = sequence
	return msg, nil
}

// buildChannelMessageMeta runs path-shaped tokens in content through
// the workspace filesystem allowlist and returns a JSON meta sidecar.
// The result is the empty string when there are no validated paths
// (pre-pathlinks behavior preserved). The frontend's markdown
// linkifier consumes the `pathRefs` key — mirrors the assistant_text
// settle-time enrichment in `internal/triage/stream_state.go`.
//
// Failures are non-fatal: a missing thread record (deleted concurrently
// with the post) or a marshal error logs and falls back to empty meta.
// Persistence must not be blocked on enrichment.
func (cs *ChannelService) buildChannelMessageMeta(threadID, content string) string {
	thread, err := cs.store.GetThread(threadID)
	if err != nil {
		log.Printf("discussion: pathlinks lookup thread %s: %v", threadID, err)
		return ""
	}
	workspacePath := strings.TrimSpace(thread.WorkspacePath)
	if workspacePath == "" {
		return ""
	}
	refs := pathlinks.ExtractAndValidate(workspacePath, content)
	if len(refs) == 0 {
		return ""
	}
	encoded, err := pathlinks.MarshalRefsJSON(refs)
	if err != nil {
		log.Printf("discussion: pathlinks marshal meta thread=%s: %v", threadID, err)
		return ""
	}
	return encoded
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

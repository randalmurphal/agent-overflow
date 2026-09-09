package claude

import (
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"time"

	"agent-overflow/internal/provider"
	"github.com/google/uuid"
)

// Message snapshots are absolute, including repeated assistant envelopes for
// separate content blocks. Retain IDs until the result so none is counted twice.
// Stop accepting new IDs at the bound; final modelUsage still accounts for all.
type claudeUsageProgress struct {
	scope          string
	segment        uint64
	activeMessage  string
	messages       map[string]provider.ModelTokenUsage
	totals         map[string]provider.TokenUsage
	closedMessages map[string]provider.ModelTokenUsage
	limitReported  bool
}

const usageProgressMessageLimit = 1024

func (p *Parser) startUsageMessage(threadID, parent string, raw json.RawMessage, now time.Time) []provider.ProviderEvent {
	if p == nil || parent != "" {
		return nil
	}
	var msg assistantMessage
	if json.Unmarshal(raw, &msg) != nil {
		return nil
	}
	p.usageProgress.activeMessage = msg.ID
	return p.reportMessageUsage(threadID, msg.ID, msg.Model, msg.Usage, now)
}

func (p *Parser) reportMessageUsage(threadID, id, model string, usage *assistantUsage, now time.Time) []provider.ProviderEvent {
	if p == nil || id == "" {
		return nil
	}
	s := &p.usageProgress
	if _, closed := s.closedMessages[id]; closed {
		return nil
	}
	previous, exists := s.messages[id]
	if !exists && (model == "" || len(s.messages) >= usageProgressMessageLimit) {
		if len(s.messages) == usageProgressMessageLimit && !s.limitReported {
			s.limitReported = true
			log.Printf("claude: live usage message limit reached for %s; awaiting result accounting", threadID)
		}
		return nil
	}
	if s.scope == "" {
		s.scope = uuid.NewString()
	}
	if s.messages == nil {
		s.messages = make(map[string]provider.ModelTokenUsage)
	}
	current := previous
	if !exists {
		current.Model = provider.NormalizeModelSlug(string(provider.Claude), model)
	}
	if usage != nil {
		current.InputTokens = max(current.InputTokens, usage.InputTokens)
		current.OutputTokens = max(current.OutputTokens, usage.OutputTokens)
		current.CacheReadInputTokens = max(current.CacheReadInputTokens, usage.CacheReadInputTokens)
		current.CacheCreationInputTokens = max(current.CacheCreationInputTokens, usage.CacheCreationInputTokens)
	}
	s.messages[id] = current
	if current == previous || current.TokenUsage.IsZero() {
		return nil
	}
	if s.totals == nil {
		s.totals = make(map[string]provider.TokenUsage)
	}
	delta := current.TokenUsage
	delta.Sub(previous.TokenUsage)
	total := s.totals[current.Model]
	total.Add(delta)
	s.totals[current.Model] = total
	models := make([]provider.ModelTokenUsage, 0, len(s.totals))
	for name, u := range s.totals {
		models = append(models, provider.ModelTokenUsage{Model: name, TokenUsage: u})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Model < models[j].Model })
	return []provider.ProviderEvent{{Kind: provider.EventUsageProgress, ThreadID: threadID, Timestamp: now,
		UsageProgress: &provider.UsageProgress{Scope: s.scope, Segment: strconv.FormatUint(s.segment, 10), ModelUsage: models}}}
}

func (p *Parser) finishUsageSegment() string {
	if p == nil {
		return ""
	}
	s := &p.usageProgress
	scope := s.scope
	s.segment++
	if len(s.messages) > 0 {
		s.closedMessages = s.messages
	}
	s.messages = nil
	s.totals = nil
	s.limitReported = false
	s.activeMessage = ""
	return scope
}

package codex

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

const (
	maxPlanDeltaBufferBytes = 256 * 1024
	maxPlanDeltaBuffers     = 16
)

type planBuffer struct {
	itemID    string
	turnID    string
	text      strings.Builder
	truncated bool
}

// classifyNotificationWithBufferedPlan classifies a notification and folds
// in any buffered plan text. The second return is the classifier's
// "did anyone claim this method?" answer, propagated so the caller can
// surface protocol drift; it is unaffected by the plan buffering, which
// only ever enriches an already-claimed item/completed.
func (s *Session) classifyNotificationWithBufferedPlan(method string, params json.RawMessage) ([]provider.ProviderEvent, bool) {
	events, handled := classifyNotification(s.threadID, method, params)
	if method != "item/completed" || classifyCodexItemType(params) != "plan" {
		return events, handled
	}
	itemID := readNestedString(params, "item", "id")
	turnID := readTopLevelString(params, "turnId")
	buffered := s.takePlanBuffer(itemID, turnID)
	if buffered == "" {
		return events, handled
	}
	for i := range events {
		if events[i].Kind == provider.EventProposedPlan {
			if events[i].Content == "" {
				events[i].Content = buffered
			}
			return events, handled
		}
	}
	return []provider.ProviderEvent{{
		Kind:      provider.EventProposedPlan,
		ThreadID:  s.threadID,
		TurnID:    turnID,
		ItemID:    itemID,
		ItemType:  "plan",
		Content:   buffered,
		Meta:      params,
		Timestamp: time.Now(),
	}}, handled
}

func (s *Session) appendPlanDelta(params json.RawMessage) {
	delta := readTopLevelString(params, "delta")
	if delta == "" {
		delta = readTopLevelString(params, "textDelta")
	}
	if delta == "" {
		return
	}
	itemID := readTopLevelString(params, "itemId")
	turnID := readTopLevelString(params, "turnId")
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := s.planBufferLocked(itemID, turnID)
	if buf.truncated {
		return
	}
	if buf.text.Len()+len(delta) > maxPlanDeltaBufferBytes {
		remaining := maxPlanDeltaBufferBytes - buf.text.Len()
		if remaining > 0 {
			buf.text.WriteString(delta[:remaining])
		}
		buf.truncated = true
		log.Printf("codex: proposed plan delta buffer exceeded %d bytes; truncating buffered fallback for thread %s", maxPlanDeltaBufferBytes, s.threadID)
		return
	}
	buf.text.WriteString(delta)
}

func (s *Session) planBufferLocked(itemID, turnID string) *planBuffer {
	if s.planBuffersByItemID == nil {
		s.planBuffersByItemID = make(map[string]*planBuffer)
	}
	if s.planBuffersByTurnID == nil {
		s.planBuffersByTurnID = make(map[string]*planBuffer)
	}
	if itemID != "" {
		if buf := s.planBuffersByItemID[itemID]; buf != nil {
			return buf
		}
	}
	if turnID != "" {
		if buf := s.planBuffersByTurnID[turnID]; buf != nil {
			if itemID != "" {
				if buf.itemID != "" && buf.itemID != itemID {
					delete(s.planBuffersByItemID, buf.itemID)
				}
				buf.itemID = itemID
				s.planBuffersByItemID[itemID] = buf
			}
			return buf
		}
	}
	if len(s.planBuffersByTurnID) >= maxPlanDeltaBuffers && turnID != "" {
		for existingTurnID, existing := range s.planBuffersByTurnID {
			if existing.itemID != "" {
				delete(s.planBuffersByItemID, existing.itemID)
			}
			delete(s.planBuffersByTurnID, existingTurnID)
			break
		}
	}
	if len(s.planBuffersByItemID) >= maxPlanDeltaBuffers && itemID != "" {
		for existingItemID, existing := range s.planBuffersByItemID {
			if existing.turnID != "" {
				delete(s.planBuffersByTurnID, existing.turnID)
			}
			delete(s.planBuffersByItemID, existingItemID)
			break
		}
	}
	buf := &planBuffer{itemID: itemID, turnID: turnID}
	if itemID != "" {
		s.planBuffersByItemID[itemID] = buf
	}
	if turnID != "" {
		s.planBuffersByTurnID[turnID] = buf
	}
	return buf
}

func (s *Session) takePlanBuffer(itemID, turnID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var buf *planBuffer
	if itemID != "" {
		buf = s.planBuffersByItemID[itemID]
	}
	if buf == nil && turnID != "" {
		buf = s.planBuffersByTurnID[turnID]
	}
	if buf == nil {
		return ""
	}
	if buf.itemID != "" {
		delete(s.planBuffersByItemID, buf.itemID)
	}
	if buf.turnID != "" {
		delete(s.planBuffersByTurnID, buf.turnID)
	}
	return buf.text.String()
}

func (s *Session) clearPlanBufferForTurn(turnID string) {
	if turnID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := s.planBuffersByTurnID[turnID]
	if buf == nil {
		return
	}
	if buf.itemID != "" {
		delete(s.planBuffersByItemID, buf.itemID)
	}
	delete(s.planBuffersByTurnID, turnID)
}

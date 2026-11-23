package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// Recovery reads bounded pages solely to recover the original spawn identity.
// It never projects unrelated transcript items or assigns a send call as a spawn.
const maxOwnershipRecoveryPages = 32

func (s *Session) scheduleChildOwnershipRecovery(childID string) {
	if !s.appServerAtLeast("0.153.4") || s.closing.Load() || s.isUnrelatedProviderThread(childID) {
		return
	}
	s.mu.Lock()
	if s.childRouting.recoveryPending == nil {
		s.childRouting.recoveryPending = make(map[string]bool)
	}
	if s.childRouting.recoveryPending[childID] || len(s.childRouting.recoveryQueue) >= 64 {
		s.mu.Unlock()
		return
	}
	s.childRouting.recoveryPending[childID] = true
	s.childRouting.recoveryQueue = append(s.childRouting.recoveryQueue, childID)
	if s.childRouting.recoveryRunning {
		s.mu.Unlock()
		return
	}
	s.childRouting.recoveryRunning = true
	s.mu.Unlock()
	s.startCollabAsync(func() {
		for {
			s.mu.Lock()
			if len(s.childRouting.recoveryQueue) == 0 || s.closing.Load() {
				s.childRouting.recoveryRunning = false
				s.mu.Unlock()
				return
			}
			id := s.childRouting.recoveryQueue[0]
			s.childRouting.recoveryQueue = s.childRouting.recoveryQueue[1:]
			s.mu.Unlock()
			ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
			err := s.recoverChildOwnership(ctx, id, make(map[string]bool))
			cancel()

			if err != nil {
				log.Printf("codex: child %s ownership recovery: %v; awaiting canonical ownership until the routing deadline", id, err)
			}
		}
	})
}

func (s *Session) recoverChildOwnership(ctx context.Context, childID string, seen map[string]bool) error {
	if childID == s.rootThreadID() || s.parentToolUseForProviderThread(childID) != "" || s.isUnrelatedProviderThread(childID) {
		return nil
	}
	if seen[childID] || len(seen) >= 16 {
		return fmt.Errorf("invalid or excessively deep child ownership chain at %s", childID)
	}
	seen[childID] = true
	response, err := s.sendRequest(ctx, "thread/read", map[string]any{"threadId": childID, "includeTurns": false})
	if err != nil {
		return err
	}
	var child struct {
		Thread struct {
			ID       string `json:"id"`
			ParentID string `json:"parentThreadId"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(response, &child); err != nil {
		return err
	}
	if child.Thread.ID != childID || child.Thread.ParentID == "" {
		return fmt.Errorf("child %s lacks verified parent metadata", childID)
	}
	parentID := child.Thread.ParentID
	if err := s.recoverChildOwnership(ctx, parentID, seen); err != nil {
		return err
	}
	cursor := ""
	for page := 0; page < maxOwnershipRecoveryPages; page++ {
		params := map[string]any{"threadId": parentID, "limit": 32, "sortDirection": "desc"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		response, err := s.sendRequest(ctx, "thread/items/list", params)
		if err != nil {
			return err
		}
		var result struct {
			Data []struct {
				TurnID string                     `json:"turnId"`
				Item   map[string]json.RawMessage `json:"item"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(response, &result); err != nil {
			return err
		}
		for _, entry := range result.Data {
			activity, ok := decodeSubAgentActivityItem(entry.Item)
			if !ok || activity.Kind != "started" || activity.AgentThreadID != childID {
				continue
			}
			if s.closing.Load() {
				return context.Canceled
			}
			if s.parentToolUseForProviderThread(childID) != "" {
				return nil
			}
			if err := s.publishRecoveredChildOwnership(parentID, childID, entry.TurnID, entry.Item, activity); err != nil {
				return err
			}
			s.mu.Lock()
			generation := s.collabHistory.generation
			s.mu.Unlock()
			s.enqueueCollabHistoryJob(collabHistoryJob{Ownership: collabHistoryOwnership{ParentItemID: activity.ItemID, ChildThreadID: childID, AgentPath: activity.AgentPath}, Generation: generation})
			return nil
		}
		if result.NextCursor == "" {
			break
		}
		if result.NextCursor == cursor {
			return fmt.Errorf("ownership history cursor did not advance")
		}
		cursor = result.NextCursor
	}
	return fmt.Errorf("original spawn for %s was not found within the ownership read limit", childID)
}

// publishRecoveredChildOwnership registers a recovered spawn's ownership and
// emits its spawn row while it holds the session's dispatch token, so the
// child's backlog, which the registration moves into the session's frame
// queue, is dispatched after the row, as it is behind a live spawn row.
func (s *Session) publishRecoveredChildOwnership(parentID, childID, turnID string, item map[string]json.RawMessage, activity subAgentActivity) error {
	if !s.takeDispatch() {
		return context.Canceled
	}
	defer s.endDispatch()
	if !s.registerHistoricalChildOwnership(parentID, childID, activity.AgentPath, activity.ItemID) {
		return fmt.Errorf("conflicting recovered ownership for %s", childID)
	}
	s.rememberSubAgentActivityProfile(activity)
	params, err := json.Marshal(map[string]any{"threadId": parentID, "turnId": turnID, "item": item})
	if err != nil {
		return err
	}
	for _, event := range classifySubAgentActivityCompleted(s.threadID, params, time.Now()) {
		event.ParentToolUseID = s.parentToolUseForProviderThread(parentID)
		s.emitEvent(event)
	}
	return nil
}

// Discover descendants on resume even if AO never received their launch event.
func (s *Session) discoverResumedChildren() {
	if !s.appServerAtLeast("0.153.4") {
		return
	}
	s.startCollabAsync(func() {
		ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
		defer cancel()
		cursor := ""
		for page := 0; page < maxOwnershipRecoveryPages; page++ {
			params := map[string]any{"ancestorThreadId": s.rootThreadID(), "sourceKinds": []string{"subAgent"}, "limit": 64, "useStateDbOnly": true}
			if cursor != "" {
				params["cursor"] = cursor
			}
			response, err := s.sendRequest(ctx, "thread/list", params)
			if err != nil {
				s.warnCollabHistory("Codex descendant discovery failed", err)
				return
			}
			var result struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
				NextCursor string `json:"nextCursor"`
			}
			if err := json.Unmarshal(response, &result); err != nil {
				s.warnCollabHistory("Codex descendant discovery could not be decoded", err)
				return
			}
			for _, child := range result.Data {
				if child.ID != "" && s.parentToolUseForProviderThread(child.ID) == "" {
					if err := s.recoverChildOwnership(ctx, child.ID, make(map[string]bool)); err != nil {
						s.warnCollabHistory("Codex descendant ownership could not be recovered", err)
					}
				}
			}
			if result.NextCursor == "" {
				return
			}
			if result.NextCursor == cursor {
				s.warnCollabHistory("Codex descendant cursor did not advance", nil)
				return
			}
			cursor = result.NextCursor
		}
		s.warnCollabHistory("Codex descendant discovery exceeded its page limit", nil)
	})
}

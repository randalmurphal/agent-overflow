package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/ctxutil"
	"agent-overflow/internal/provider"
)

const maxCollabHistoryThreads = 256

type collabHistoryOwnership struct {
	SourceThreadID string
	ParentItemID   string
	ChildThreadID  string
	AgentPath      string
	LaunchMeta     collabLaunchMeta
}

type collabHistoryJob struct {
	Ownership  collabHistoryOwnership
	Generation uint64
}

// rehydrateCollabOwnershipFromThreadResponse rebuilds provider-side routing
// indexes from the root thread/resume history without replaying the transcript.
// Historical spawn rows were already projected into SQLite when they first
// arrived. Descendant histories are inspected with read-only thread/read calls:
// active children are resumed to restore live notification subscriptions, and
// terminal children repair spawn state that AO may have missed while offline.
func (s *Session) rehydrateCollabOwnershipFromThreadResponse(resp json.RawMessage) {
	ownerships, err := collabHistoryOwnerships(resp)
	if err != nil {
		s.warnCollabHistory("Codex collaboration history could not be decoded completely", err)
	}
	if len(ownerships) == 0 {
		return
	}
	if len(ownerships) > maxCollabHistoryThreads {
		s.warnCollabHistory("Codex collaboration history exceeded the safe traversal limit", nil)
		ownerships = ownerships[:maxCollabHistoryThreads]
	}

	s.mu.Lock()
	generation := s.collabHistoryGeneration
	s.mu.Unlock()
	mapped := make([]string, 0, len(ownerships))
	for _, ownership := range ownerships {
		if !s.registerHistoricalChildOwnership(ownership.SourceThreadID, ownership.ChildThreadID, ownership.AgentPath, ownership.ParentItemID) {
			continue
		}
		s.scheduleCollabMetadataRead(ownership.ChildThreadID, ownership.ParentItemID, ownership.LaunchMeta)
		mapped = append(mapped, ownership.ChildThreadID)
		s.enqueueCollabHistoryJob(collabHistoryJob{Ownership: ownership, Generation: generation})
	}
	// Persisted spawn rows already exist on a reopen. Drain only after every
	// ownership edge in this response has been registered.
	s.drainDeferredChildWireEvents(mapped...)
}

func collabHistoryOwnerships(resp json.RawMessage) ([]collabHistoryOwnership, error) {
	var response struct {
		Thread struct {
			ID    string `json:"id"`
			Turns []struct {
				ID    string            `json:"id"`
				Items []json.RawMessage `json:"items"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if len(resp) == 0 {
		return nil, errors.New("empty thread history response")
	}
	if err := json.Unmarshal(resp, &response); err != nil {
		return nil, fmt.Errorf("decode thread history response: %w", err)
	}
	sourceThreadID := strings.TrimSpace(response.Thread.ID)
	if sourceThreadID == "" {
		return nil, errors.New("thread history response is missing thread.id")
	}

	var ownerships []collabHistoryOwnership
	var parseErrors []error
	for _, turn := range response.Thread.Turns {
		for itemIndex, rawItem := range turn.Items {
			var item map[string]json.RawMessage
			if err := json.Unmarshal(rawItem, &item); err != nil {
				parseErrors = append(parseErrors, fmt.Errorf("turn %s item %d: %w", turn.ID, itemIndex, err))
				continue
			}
			itemType := readRawString(item, "type")
			if itemType == "subAgentActivity" {
				activity, ok := decodeSubAgentActivityItem(item)
				if !ok {
					if readRawString(item, "kind") == "started" {
						parseErrors = append(parseErrors, fmt.Errorf("turn %s item %d: malformed started subAgentActivity", turn.ID, itemIndex))
					}
					continue
				}
				if activity.Kind != "started" {
					continue
				}
				ownerships = append(ownerships, collabHistoryOwnership{
					SourceThreadID: sourceThreadID,
					ParentItemID:   activity.ItemID,
					ChildThreadID:  activity.AgentThreadID,
					AgentPath:      activity.AgentPath,
					LaunchMeta: collabLaunchMeta{
						AgentPath:         activity.AgentPath,
						ReceiverThreadIDs: []string{activity.AgentThreadID},
					},
				})
				continue
			}
			if itemType != "collabAgentToolCall" ||
				normalizeCollabToolName(readRawString(item, "tool")) != "spawn_agent" {
				continue
			}
			parentItemID := strings.TrimSpace(readRawString(item, "id"))
			receivers := readRawStringArray(item, "receiverThreadIds")
			if parentItemID == "" || len(receivers) == 0 {
				parseErrors = append(parseErrors, fmt.Errorf("turn %s item %d: malformed spawn_agent history item", turn.ID, itemIndex))
				continue
			}
			launchMeta := collabLaunchMeta{
				Prompt:            readRawString(item, "prompt"),
				Model:             readRawString(item, "model"),
				ReasoningEffort:   readRawString(item, "reasoningEffort"),
				ReceiverThreadIDs: append([]string(nil), receivers...),
			}
			for _, childThreadID := range receivers {
				ownerships = append(ownerships, collabHistoryOwnership{
					SourceThreadID: sourceThreadID,
					ParentItemID:   parentItemID,
					ChildThreadID:  strings.TrimSpace(childThreadID),
					LaunchMeta:     launchMeta,
				})
			}
		}
	}
	return ownerships, errors.Join(parseErrors...)
}

func (s *Session) beginCollabHistoryGeneration() {
	s.mu.Lock()
	s.collabHistoryGeneration++
	s.collabHistoryQueue = nil
	s.collabHistoryVisited = make(map[string]uint64)
	s.collabHistoryAttempts = 0
	s.mu.Unlock()
}

func (s *Session) enqueueCollabHistoryJob(job collabHistoryJob) {
	if strings.TrimSpace(job.Ownership.ChildThreadID) == "" || s.closing.Load() {
		return
	}
	s.mu.Lock()
	if job.Generation != s.collabHistoryGeneration {
		s.mu.Unlock()
		return
	}
	if len(s.collabHistoryQueue) >= maxCollabHistoryThreads {
		s.mu.Unlock()
		s.warnCollabHistory("Codex collaboration history exceeded the safe traversal limit", nil)
		return
	}
	s.collabHistoryQueue = append(s.collabHistoryQueue, job)
	if s.collabHistoryRunning {
		s.mu.Unlock()
		return
	}
	s.collabHistoryRunning = true
	s.mu.Unlock()

	if !s.startCollabAsync(s.runCollabHistoryQueue) {
		s.mu.Lock()
		s.collabHistoryRunning = false
		s.mu.Unlock()
	}
}

func (s *Session) runCollabHistoryQueue() {
	for {
		s.mu.Lock()
		if len(s.collabHistoryQueue) == 0 {
			s.collabHistoryRunning = false
			s.mu.Unlock()
			return
		}
		job := s.collabHistoryQueue[0]
		s.collabHistoryQueue[0] = collabHistoryJob{}
		s.collabHistoryQueue = s.collabHistoryQueue[1:]
		childThreadID := strings.TrimSpace(job.Ownership.ChildThreadID)
		if job.Generation != s.collabHistoryGeneration || s.collabHistoryVisited[childThreadID] == job.Generation {
			s.mu.Unlock()
			continue
		}
		if s.collabHistoryAttempts >= maxCollabHistoryThreads {
			s.mu.Unlock()
			s.warnCollabHistory("Codex collaboration history exceeded the safe traversal limit", nil)
			continue
		}
		s.collabHistoryAttempts++
		s.collabHistoryVisited[childThreadID] = job.Generation
		s.mu.Unlock()

		if err := s.inspectCollabHistoryChild(job); err != nil {
			s.mu.Lock()
			if s.collabHistoryVisited[childThreadID] == job.Generation {
				delete(s.collabHistoryVisited, childThreadID)
			}
			s.mu.Unlock()
			s.warnCollabHistory(fmt.Sprintf("Codex child history %s could not be inspected", childThreadID), err)
		}
	}
}

func (s *Session) inspectCollabHistoryChild(job collabHistoryJob) error {
	lifecycleRevision := s.childLifecycleRevisionForThread(job.Ownership.ChildThreadID)
	resp, err := s.readCollabHistoryWithRetry(job.Ownership.ChildThreadID)
	if err != nil {
		return err
	}
	if !s.collabHistoryGenerationCurrent(job.Generation) {
		return nil
	}
	status, err := s.reconcileCollabHistoryTerminal(job, resp, lifecycleRevision)
	if err != nil {
		return err
	}
	if status == "active" {
		if err := s.attachActiveChildWithRetry(job.Ownership.ChildThreadID); err != nil {
			return err
		}
	}
	ownerships, parseErr := collabHistoryOwnerships(resp)
	mapped := make([]string, 0, len(ownerships))
	for _, ownership := range ownerships {
		if !s.collabHistoryGenerationCurrent(job.Generation) {
			return nil
		}
		if !s.registerHistoricalChildOwnership(ownership.SourceThreadID, ownership.ChildThreadID, ownership.AgentPath, ownership.ParentItemID) {
			continue
		}
		s.scheduleCollabMetadataRead(ownership.ChildThreadID, ownership.ParentItemID, ownership.LaunchMeta)
		mapped = append(mapped, ownership.ChildThreadID)
		s.enqueueCollabHistoryJob(collabHistoryJob{Ownership: ownership, Generation: job.Generation})
	}
	s.drainDeferredChildWireEvents(mapped...)
	if parseErr != nil {
		s.warnCollabHistory(fmt.Sprintf("Codex child history %s could not be decoded completely", job.Ownership.ChildThreadID), parseErr)
	}
	return nil
}

// reconcileCollabHistoryTerminal repairs a persisted spawn that missed its
// child-scoped turn/completed notification while AO was disconnected. Active
// children are deliberately excluded: their latest stored turn may be an older
// completed turn while a newer turn is still running.
func (s *Session) reconcileCollabHistoryTerminal(job collabHistoryJob, resp json.RawMessage, expectedLifecycleRevision uint64) (string, error) {
	providerThreadID := strings.TrimSpace(job.Ownership.ChildThreadID)
	parentToolUseID := strings.TrimSpace(job.Ownership.ParentItemID)
	if providerThreadID == "" || parentToolUseID == "" {
		return "", errors.New("child terminal history job is missing ownership identifiers")
	}
	var response struct {
		Thread struct {
			ID     string `json:"id"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
			Turns []struct {
				Status string `json:"status"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(resp, &response); err != nil {
		return "", fmt.Errorf("decode child terminal history: %w", err)
	}
	responseThreadID := strings.TrimSpace(response.Thread.ID)
	if responseThreadID == "" || responseThreadID != providerThreadID {
		return "", fmt.Errorf("child terminal history thread mismatch: got %q, want %q", responseThreadID, providerThreadID)
	}
	var status string
	switch response.Thread.Status.Type {
	case "active":
		return response.Thread.Status.Type, nil
	case "systemError":
		status = "errored"
	case "idle", "notLoaded":
		if len(response.Thread.Turns) == 0 {
			return response.Thread.Status.Type, nil
		}
		latestTurn := response.Thread.Turns[len(response.Thread.Turns)-1]
		status = codexSubagentStatusFromTurnStatus(latestTurn.Status)
		if status == "" {
			return response.Thread.Status.Type, nil
		}
	default:
		return "", fmt.Errorf("child terminal history has unknown thread status %q", response.Thread.Status.Type)
	}
	event := s.childStatusEvent(providerThreadID, parentToolUseID, status)
	if event == nil {
		return "", errors.New("could not construct recovered child terminal status")
	}

	s.emitRecoveredChildStatus(providerThreadID, expectedLifecycleRevision, *event)
	return response.Thread.Status.Type, nil
}

func (s *Session) readCollabHistoryWithRetry(providerThreadID string) (json.RawMessage, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 && !ctxutil.Sleep(s.ctx, time.Duration(attempt)*150*time.Millisecond) {
			return nil, s.ctx.Err()
		}
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		resp, err := s.sendRequest(ctx, "thread/read", map[string]any{
			"threadId":     providerThreadID,
			"includeTurns": true,
		})
		cancel()
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (s *Session) attachActiveChildWithRetry(providerThreadID string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 && !ctxutil.Sleep(s.ctx, time.Duration(attempt)*150*time.Millisecond) {
			return s.ctx.Err()
		}
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		_, err := s.sendRequest(ctx, "thread/resume", map[string]any{
			"threadId":     providerThreadID,
			"excludeTurns": true,
		})
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

func (s *Session) collabHistoryGenerationCurrent(generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return generation == s.collabHistoryGeneration && !s.closing.Load()
}

func (s *Session) startCollabAsync(work func()) bool {
	s.collabAsyncMu.Lock()
	defer s.collabAsyncMu.Unlock()
	if s.collabAsyncClosing {
		return false
	}
	s.collabAsyncWG.Add(1)
	go func() {
		defer s.collabAsyncWG.Done()
		work()
	}()
	return true
}

func (s *Session) warnCollabHistory(message string, err error) {
	s.mu.Lock()
	generation := s.collabHistoryGeneration
	if s.collabHistoryWarnedGeneration == generation {
		s.mu.Unlock()
		if err != nil {
			log.Printf("codex: %s: %v", message, err)
		}
		return
	}
	s.collabHistoryWarnedGeneration = generation
	s.mu.Unlock()

	if err != nil {
		log.Printf("codex: %s: %v", message, err)
	} else {
		log.Printf("codex: %s", message)
	}
	if s.onEvent == nil || s.closing.Load() {
		return
	}
	meta, marshalErr := json.Marshal(map[string]string{
		"kind":  "warning",
		"title": "Subagent history warning",
	})
	if marshalErr != nil {
		meta = json.RawMessage(`{"kind":"warning","title":"Subagent history warning"}`)
	}
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventNotification,
		ThreadID:  s.threadID,
		Content:   message,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

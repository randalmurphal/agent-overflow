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

type collabHistoryOwnership struct {
	ParentItemID  string
	ChildThreadID string
	AgentPath     string
	LaunchMeta    collabLaunchMeta
}

type collabHistoryJob struct {
	Ownership  collabHistoryOwnership
	Generation uint64
}

// rehydrateCollabOwnership rebuilds provider-side routing indexes from AO's
// compact persisted spawn rows. The transcript itself never crosses the resume
// boundary: AO already projected it into SQLite when it arrived, and Codex can
// represent an arbitrarily long session as one turn and one JSON-RPC frame.
// Child metadata is inspected separately below so active subscriptions and a
// terminal status missed while AO was offline are still recovered.
func (s *Session) rehydrateCollabOwnership(launches []ResumeCollabLaunch) {
	ownerships, err := collabResumeOwnerships(launches)
	if err != nil {
		s.warnCollabHistory("Persisted Codex collaboration ownership could not be decoded completely", err)
	}
	if len(ownerships) == 0 {
		return
	}
	s.mu.Lock()
	generation := s.collabHistory.generation
	s.mu.Unlock()
	mapped := make([]string, 0, len(ownerships))
	for _, ownership := range ownerships {
		if !s.registerHistoricalChildOwnership("", ownership.ChildThreadID, ownership.AgentPath, ownership.ParentItemID) {
			continue
		}
		s.scheduleCollabMetadataRead(ownership.ChildThreadID, ownership.ParentItemID, ownership.LaunchMeta)
		mapped = append(mapped, ownership.ChildThreadID)
		s.enqueueCollabHistoryJob(collabHistoryJob{Ownership: ownership, Generation: generation})
	}
	// Persisted spawn rows already exist on a reopen. Drain only after every
	// ownership edge in this resume set has been registered.
	s.drainDeferredChildWireEvents(mapped...)
}

func collabResumeOwnerships(launches []ResumeCollabLaunch) ([]collabHistoryOwnership, error) {
	var ownerships []collabHistoryOwnership
	var parseErrors []error
	for launchIndex, launch := range launches {
		parentItemID := strings.TrimSpace(launch.ItemID)
		var stored struct {
			Input struct {
				Tool              string   `json:"tool"`
				Prompt            string   `json:"prompt"`
				Model             string   `json:"model"`
				ReasoningEffort   string   `json:"reasoningEffort"`
				AgentPath         string   `json:"agentPath"`
				ReceiverThreadIDs []string `json:"receiverThreadIds"`
			} `json:"input"`
		}
		if err := json.Unmarshal(launch.Meta, &stored); err != nil {
			parseErrors = append(parseErrors, fmt.Errorf("launch %d (%s): decode meta: %w", launchIndex, parentItemID, err))
			continue
		}
		if parentItemID == "" || normalizeCollabToolName(stored.Input.Tool) != "spawn_agent" {
			parseErrors = append(parseErrors, fmt.Errorf("launch %d (%s): missing spawn ownership identity", launchIndex, parentItemID))
			continue
		}
		receivers := nonEmptyStrings(stored.Input.ReceiverThreadIDs)
		if len(receivers) == 0 {
			parseErrors = append(parseErrors, fmt.Errorf("launch %d (%s): missing receiver thread ids", launchIndex, parentItemID))
			continue
		}
		agentPath := strings.TrimSpace(stored.Input.AgentPath)
		launchMeta := collabLaunchMeta{
			Prompt:            stored.Input.Prompt,
			Model:             stored.Input.Model,
			ReasoningEffort:   stored.Input.ReasoningEffort,
			AgentPath:         agentPath,
			ReceiverThreadIDs: append([]string(nil), receivers...),
		}
		for _, childThreadID := range receivers {
			childAgentPath := ""
			if len(receivers) == 1 {
				childAgentPath = agentPath
			}
			ownerships = append(ownerships, collabHistoryOwnership{
				ParentItemID:  parentItemID,
				ChildThreadID: childThreadID,
				AgentPath:     childAgentPath,
				LaunchMeta:    launchMeta,
			})
		}
	}
	return ownerships, errors.Join(parseErrors...)
}

func (s *Session) enqueueCollabHistoryJob(job collabHistoryJob) {
	if strings.TrimSpace(job.Ownership.ChildThreadID) == "" || s.closing.Load() {
		return
	}
	s.mu.Lock()
	if job.Generation != s.collabHistory.generation {
		s.mu.Unlock()
		return
	}
	s.collabHistory.queue = append(s.collabHistory.queue, job)
	if s.collabHistory.running {
		s.mu.Unlock()
		return
	}
	s.collabHistory.running = true
	s.mu.Unlock()

	if !s.startCollabAsync(s.runCollabHistoryQueue) {
		s.mu.Lock()
		s.collabHistory.running = false
		s.mu.Unlock()
	}
}

func (s *Session) runCollabHistoryQueue() {
	for {
		s.mu.Lock()
		if len(s.collabHistory.queue) == 0 {
			s.collabHistory.running = false
			s.mu.Unlock()
			return
		}
		job := s.collabHistory.queue[0]
		s.collabHistory.queue[0] = collabHistoryJob{}
		s.collabHistory.queue = s.collabHistory.queue[1:]
		childThreadID := strings.TrimSpace(job.Ownership.ChildThreadID)
		if job.Generation != s.collabHistory.generation || s.collabHistory.visited[childThreadID] == job.Generation {
			s.mu.Unlock()
			continue
		}
		s.collabHistory.visited[childThreadID] = job.Generation
		s.mu.Unlock()

		if err := s.inspectCollabHistoryChild(job); err != nil {
			s.mu.Lock()
			if s.collabHistory.visited[childThreadID] == job.Generation {
				delete(s.collabHistory.visited, childThreadID)
			}
			s.mu.Unlock()
			s.warnCollabHistory(fmt.Sprintf("Codex child recovery metadata %s could not be inspected", childThreadID), err)
		}
	}
}

func (s *Session) inspectCollabHistoryChild(job collabHistoryJob) error {
	lifecycleRevision := s.childLifecycleRevisionForThread(job.Ownership.ChildThreadID)
	snapshot, err := s.readCollabHistoryWithRetry(job.Ownership.ChildThreadID)
	if err != nil {
		return err
	}
	if !s.collabHistoryGenerationCurrent(job.Generation) {
		return nil
	}
	status, err := s.reconcileCollabHistoryTerminal(job, snapshot, lifecycleRevision)
	if err != nil {
		return err
	}
	if status == "active" {
		if err := s.attachActiveChildWithRetry(job.Ownership.ChildThreadID); err != nil {
			return err
		}
	}
	return nil
}

type collabThreadSnapshot struct {
	ThreadID         string
	Status           string
	LatestTurnStatus string
}

// reconcileCollabHistoryTerminal repairs a persisted spawn that missed its
// child-scoped turn/completed notification while AO was disconnected. Active
// children are deliberately excluded: their latest stored turn may be an older
// completed turn while a newer turn is still running.
func (s *Session) reconcileCollabHistoryTerminal(job collabHistoryJob, snapshot collabThreadSnapshot, expectedLifecycleRevision uint64) (string, error) {
	providerThreadID := strings.TrimSpace(job.Ownership.ChildThreadID)
	parentToolUseID := strings.TrimSpace(job.Ownership.ParentItemID)
	if providerThreadID == "" || parentToolUseID == "" {
		return "", errors.New("child recovery job is missing ownership identifiers")
	}
	responseThreadID := strings.TrimSpace(snapshot.ThreadID)
	if responseThreadID == "" || responseThreadID != providerThreadID {
		return "", fmt.Errorf("child recovery snapshot thread mismatch: got %q, want %q", responseThreadID, providerThreadID)
	}
	var status string
	switch snapshot.Status {
	case "active":
		return snapshot.Status, nil
	case "systemError":
		status = "errored"
	case "idle", "notLoaded":
		if snapshot.LatestTurnStatus == "" {
			return snapshot.Status, nil
		}
		status = codexSubagentStatusFromTurnStatus(snapshot.LatestTurnStatus)
		if status == "" {
			return snapshot.Status, nil
		}
	default:
		return "", fmt.Errorf("child recovery snapshot has unknown thread status %q", snapshot.Status)
	}
	event := s.childStatusEvent(providerThreadID, parentToolUseID, status)
	if event == nil {
		return "", errors.New("could not construct recovered child terminal status")
	}

	s.emitRecoveredChildStatus(providerThreadID, expectedLifecycleRevision, *event)
	return snapshot.Status, nil
}

func (s *Session) readCollabHistoryWithRetry(providerThreadID string) (collabThreadSnapshot, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 && !ctxutil.Sleep(s.ctx, time.Duration(attempt)*150*time.Millisecond) {
			return collabThreadSnapshot{}, s.ctx.Err()
		}
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		snapshot, err := s.readCollabThreadSnapshot(ctx, providerThreadID)
		cancel()
		if err == nil {
			return snapshot, nil
		}
		lastErr = err
	}
	return collabThreadSnapshot{}, lastErr
}

func (s *Session) readCollabThreadSnapshot(ctx context.Context, providerThreadID string) (collabThreadSnapshot, error) {
	resp, err := s.sendRequest(ctx, "thread/read", map[string]any{
		"threadId":     providerThreadID,
		"includeTurns": false,
	})
	if err != nil {
		return collabThreadSnapshot{}, err
	}
	var read struct {
		Thread struct {
			ID     string `json:"id"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(resp, &read); err != nil {
		return collabThreadSnapshot{}, fmt.Errorf("decode child thread metadata: %w", err)
	}
	snapshot := collabThreadSnapshot{
		ThreadID: strings.TrimSpace(read.Thread.ID),
		Status:   strings.TrimSpace(read.Thread.Status.Type),
	}
	if snapshot.ThreadID == "" || snapshot.Status == "" {
		return collabThreadSnapshot{}, errors.New("child thread metadata is missing id or status")
	}
	if snapshot.Status != "idle" && snapshot.Status != "notLoaded" {
		return snapshot, nil
	}

	turnsResp, err := s.sendRequest(ctx, threadTurnsListMethod, map[string]any{
		"threadId":      providerThreadID,
		"limit":         1,
		"sortDirection": "desc",
		"itemsView":     "notLoaded",
	})
	if err != nil {
		return collabThreadSnapshot{}, err
	}
	var turns struct {
		Data []struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(turnsResp, &turns); err != nil {
		return collabThreadSnapshot{}, fmt.Errorf("decode child latest turn: %w", err)
	}
	if len(turns.Data) > 0 {
		snapshot.LatestTurnStatus = strings.TrimSpace(turns.Data[0].Status)
	}
	return snapshot, nil
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
	return generation == s.collabHistory.generation && !s.closing.Load()
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
	generation := s.collabHistory.generation
	if s.collabHistory.warnedGeneration == generation {
		s.mu.Unlock()
		if err != nil {
			log.Printf("codex: %s: %v", message, err)
		}
		return
	}
	s.collabHistory.warnedGeneration = generation
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
		"title": "Subagent recovery warning",
	})
	if marshalErr != nil {
		meta = json.RawMessage(`{"kind":"warning","title":"Subagent recovery warning"}`)
	}
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventNotification,
		ThreadID:  s.threadID,
		Content:   message,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

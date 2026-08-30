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

// scheduleCollabProfileRead resolves the effective profile from the child
// thread itself. MultiAgentV2 activity items identify the child but omit its
// model and reasoning effort. The spawn arguments are only requested values:
// Codex can change them through defaults and role configuration before it
// creates the child.
func (s *Session) scheduleCollabProfileRead(providerThreadID, parentToolUseID string, launchMeta collabLaunchMeta) {
	providerThreadID = strings.TrimSpace(providerThreadID)
	parentToolUseID = strings.TrimSpace(parentToolUseID)
	if s.proc == nil || providerThreadID == "" || parentToolUseID == "" {
		return
	}
	if !s.beginCollabProfileRead(providerThreadID) {
		return
	}
	if !s.startCollabAsync(func() {
		defer s.finishCollabProfileRead(providerThreadID)
		ctx := s.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if !s.acquireCollabProfileRead(ctx) {
			return
		}
		defer s.releaseCollabMetadataRead()

		meta, err := s.readChildThreadProfileWithRetry(ctx, providerThreadID)
		if err != nil {
			if !s.closing.Load() && s.collabProfileReadStillOwned(providerThreadID, parentToolUseID) {
				s.warnCollabProfile(parentToolUseID, err)
			}
			return
		}
		if s.closing.Load() || !s.collabProfileReadStillOwned(providerThreadID, parentToolUseID) {
			return
		}
		s.rememberCollabReceiverMeta(meta)
		s.emitCollabReceiverMetaUpdate(parentToolUseID, meta, launchMeta)
	}) {
		s.finishCollabProfileRead(providerThreadID)
		return
	}
}

func (s *Session) beginCollabProfileRead(providerThreadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.collab.agentMetaByThread[providerThreadID].ProfileKnown {
		return false
	}
	if s.collab.profileReadsByThread == nil {
		s.collab.profileReadsByThread = make(map[string]struct{})
	}
	if _, exists := s.collab.profileReadsByThread[providerThreadID]; exists {
		return false
	}
	s.collab.profileReadsByThread[providerThreadID] = struct{}{}
	return true
}

func (s *Session) finishCollabProfileRead(providerThreadID string) {
	s.mu.Lock()
	delete(s.collab.profileReadsByThread, providerThreadID)
	s.mu.Unlock()
}

func (s *Session) collabProfileReadStillOwned(providerThreadID, parentToolUseID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.collab.childParentByThread[providerThreadID] == parentToolUseID
}

// acquireCollabProfileRead waits inside a tracked background goroutine rather
// than dropping the request when all metadata slots are busy. A dropped read
// would leave the child profile blank for the rest of the session.
func (s *Session) acquireCollabProfileRead(ctx context.Context) bool {
	if s.collabMetadataReads == nil {
		return true
	}
	select {
	case s.collabMetadataReads <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Session) readChildThreadProfileWithRetry(ctx context.Context, providerThreadID string) (collabReceiverMeta, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 && !ctxutil.Sleep(ctx, time.Duration(attempt)*100*time.Millisecond) {
			return collabReceiverMeta{}, ctx.Err()
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		meta, err := s.readChildThreadProfileOnce(attemptCtx, providerThreadID)
		cancel()
		if err == nil {
			return meta, nil
		}
		lastErr = err
		if s.closing.Load() {
			return collabReceiverMeta{}, context.Canceled
		}
	}
	return collabReceiverMeta{}, lastErr
}

func (s *Session) readChildThreadProfileOnce(ctx context.Context, providerThreadID string) (collabReceiverMeta, error) {
	providerThreadID = strings.TrimSpace(providerThreadID)
	if providerThreadID == "" {
		return collabReceiverMeta{}, errors.New("child profile read is missing the thread id")
	}
	resp, err := s.sendRequest(ctx, "thread/resume", map[string]any{
		"threadId":     providerThreadID,
		"excludeTurns": true,
	})
	if err != nil {
		return collabReceiverMeta{}, err
	}
	var decoded struct {
		Thread struct {
			ID            string `json:"id"`
			AgentNickname string `json:"agentNickname"`
			AgentRole     string `json:"agentRole"`
		} `json:"thread"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoningEffort"`
	}
	if err := json.Unmarshal(resp, &decoded); err != nil {
		return collabReceiverMeta{}, fmt.Errorf("decode child thread/resume response: %w", err)
	}
	responseThreadID := strings.TrimSpace(decoded.Thread.ID)
	if responseThreadID != providerThreadID {
		return collabReceiverMeta{}, fmt.Errorf("child profile thread mismatch: got %q, want %q", responseThreadID, providerThreadID)
	}
	model := strings.TrimSpace(decoded.Model)
	if model == "" {
		return collabReceiverMeta{}, errors.New("child thread/resume response is missing model")
	}
	return collabReceiverMeta{
		ThreadID:        responseThreadID,
		AgentNickname:   strings.TrimSpace(decoded.Thread.AgentNickname),
		AgentRole:       strings.TrimSpace(decoded.Thread.AgentRole),
		Model:           model,
		ReasoningEffort: strings.TrimSpace(decoded.ReasoningEffort),
		ProfileKnown:    true,
	}, nil
}

func (s *Session) warnCollabProfile(parentToolUseID string, err error) {
	s.mu.Lock()
	if s.collab.profileWarningEmitted {
		s.mu.Unlock()
		log.Printf("codex: read effective child profile for spawn %s: %v", parentToolUseID, err)
		return
	}
	s.collab.profileWarningEmitted = true
	s.mu.Unlock()

	log.Printf("codex: read effective child profile for spawn %s: %v", parentToolUseID, err)
	if s.onEvent == nil || s.closing.Load() {
		return
	}
	meta, marshalErr := json.Marshal(map[string]string{
		"kind":  "warning",
		"title": "Subagent profile warning",
	})
	if marshalErr != nil {
		meta = json.RawMessage(`{"kind":"warning","title":"Subagent profile warning"}`)
	}
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventNotification,
		ThreadID:  s.threadID,
		Content:   "Codex did not report this subagent's effective model. Agent Overflow left the model blank rather than showing the parent model.",
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

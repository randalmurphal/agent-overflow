package codex

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/provider"
)

type childInterruptTarget struct {
	threadID   string
	turnID     string
	generation uint64
}

// InterruptSubagent stops the live child work owned directly by launchID.
// Codex has no client close_agent RPC. The supported client primitive is
// turn/interrupt against the child provider thread. An empty turn id is the
// upstream startup-interrupt form for a child whose turn has not started yet.
//
// The launch id is resolved through the session's typed ownership map. A
// caller cannot supply an arbitrary provider thread id and stop an unrelated
// root, ancestor, or sibling. false, nil means the owned child was already
// terminal in this live app-server process.
func (s *Session) InterruptSubagent(ctx context.Context, launchID string) (bool, error) {
	launchID = strings.TrimSpace(launchID)
	if launchID == "" {
		return false, errors.New("codex: interrupt subagent: launch id required")
	}

	s.mu.Lock()
	owned := false
	targets := make([]childInterruptTarget, 0, 1)
	for childThreadID, parentToolUseID := range s.collab.childParentByThread {
		if parentToolUseID != launchID {
			continue
		}
		owned = true
		runtime := s.collab.childRuntimeByThread[childThreadID]
		if runtime.phase != childRuntimePending && runtime.phase != childRuntimeRunning {
			continue
		}
		targets = append(targets, childInterruptTarget{
			threadID:   childThreadID,
			turnID:     runtime.turnID,
			generation: runtime.generation,
		})
		runtime.phase = childRuntimeStopping
		s.collab.childRuntimeByThread[childThreadID] = runtime
	}
	s.mu.Unlock()

	if !owned {
		return false, fmt.Errorf("codex: interrupt subagent: launch %s is not owned by this session", launchID)
	}
	if len(targets) == 0 {
		return false, nil
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].threadID < targets[j].threadID })

	var stopErrors []error
	stoppedAny := false
	for _, target := range targets {
		err := s.interruptChildTurn(ctx, target.threadID, target.turnID)
		if err != nil {
			s.restoreChildAfterFailedInterrupt(target)
			stopErrors = append(stopErrors, fmt.Errorf("child %s: %w", target.threadID, err))
			continue
		}
		stoppedAny = true
		if s.finishChildInterrupt(target) {
			s.drainPendingApprovalsForScope(target.threadID, "cancel", true)
			if event := s.childStatusEvent(target.threadID, launchID, "interrupted"); event != nil {
				s.observeAndEmitChildLifecycle(target.threadID, []provider.ProviderEvent{*event})
			}
		}
	}
	return stoppedAny, errors.Join(stopErrors...)
}

func (s *Session) interruptChildTurn(ctx context.Context, childThreadID, turnID string) error {
	if s.interruptChildTurnFn != nil {
		return s.interruptChildTurnFn(ctx, childThreadID, turnID)
	}
	_, err := s.sendRequest(ctx, "turn/interrupt", map[string]any{
		"threadId": childThreadID,
		"turnId":   turnID,
	})
	return err
}

func (s *Session) restoreChildAfterFailedInterrupt(target childInterruptTarget) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runtime := s.collab.childRuntimeByThread[target.threadID]
	if runtime.phase != childRuntimeStopping || runtime.generation != target.generation {
		return
	}
	if target.turnID == "" {
		runtime.phase = childRuntimePending
	} else {
		runtime.phase = childRuntimeRunning
	}
	s.collab.childRuntimeByThread[target.threadID] = runtime
}

// finishChildInterrupt returns false when a newer child turn won the race
// while the RPC was in flight. The old turn stopped, but the launch remains
// live and must not receive a synthetic terminal status.
func (s *Session) finishChildInterrupt(target childInterruptTarget) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	runtime := s.collab.childRuntimeByThread[target.threadID]
	if target.turnID != "" && runtime.generation != target.generation {
		return false
	}
	runtime.phase = childRuntimeStopped
	runtime.turnID = ""
	s.collab.childRuntimeByThread[target.threadID] = runtime
	return true
}

func (s *Session) recordChildTurnStarted(providerThreadID, turnID string) {
	providerThreadID = strings.TrimSpace(providerThreadID)
	if providerThreadID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.collab.childRuntimeByThread == nil {
		s.collab.childRuntimeByThread = make(map[string]childRuntimeState)
	}
	runtime := s.collab.childRuntimeByThread[providerThreadID]
	runtime.phase = childRuntimeRunning
	runtime.turnID = strings.TrimSpace(turnID)
	runtime.generation++
	s.collab.childRuntimeByThread[providerThreadID] = runtime
}

func (s *Session) recordChildTurnCompleted(providerThreadID, turnID string) {
	providerThreadID = strings.TrimSpace(providerThreadID)
	if providerThreadID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	runtime := s.collab.childRuntimeByThread[providerThreadID]
	turnID = strings.TrimSpace(turnID)
	if runtime.turnID != "" && turnID != "" && runtime.turnID != turnID {
		return
	}
	runtime.phase = childRuntimeStopped
	runtime.turnID = ""
	s.collab.childRuntimeByThread[providerThreadID] = runtime
}

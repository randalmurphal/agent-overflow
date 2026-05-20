package codex

import (
	"context"
	"encoding/json"
	"fmt"
)

// ThreadRollbackResult is the subset of Codex's ThreadRollbackResponse that
// Agent Overflow needs to align local state after a successful rollback.
type ThreadRollbackResult struct {
	ThreadID  string
	TurnCount int
}

// Rollback asks the Codex app-server to drop the last numTurns from the active
// thread's conversation history and persist a rollback marker in the rollout
// so future resumes see the pruned history. numTurns must be >= 1.
//
// This is a conversation-only operation by protocol design: Codex does not
// touch local file changes that the agent may have made. Callers that also
// want the working tree restored must pair this with a checkpoint restore
// separately (see app_checkpoint.go for the caller-side orchestration).
//
// Wire contract is owned by the Codex source of truth:
// /home/rmurphy/repos/codex/codex-rs/app-server-protocol/schema/typescript/v2/ThreadRollbackParams.ts
func (s *Session) Rollback(ctx context.Context, numTurns int) (ThreadRollbackResult, error) {
	return s.RollbackThread(ctx, s.codexThreadID, numTurns)
}

// RollbackThread is like Rollback but targets an explicit threadID rather
// than the session's own bound thread. Required for fork-at-point: after
// `thread/fork` returns a new forkID, the caller issues
// `thread/rollback(forkID, N)` over the SAME stdio session — the
// app-server routes per-threadID via its in-memory thread_manager
// (verified by spike at /tmp/spike-codex-fork/), so this writes the
// `ThreadRolledBack` marker into the FORK's rollout, not the source's.
func (s *Session) RollbackThread(ctx context.Context, threadID string, numTurns int) (ThreadRollbackResult, error) {
	if threadID == "" {
		return ThreadRollbackResult{}, fmt.Errorf("codex: thread/rollback: threadID is empty")
	}
	if numTurns < 1 {
		return ThreadRollbackResult{}, fmt.Errorf("codex: thread/rollback: numTurns must be >= 1, got %d", numTurns)
	}
	resp, err := s.sendRequest(ctx, "thread/rollback", map[string]any{
		"threadId": threadID,
		"numTurns": numTurns,
	})
	if err != nil {
		return ThreadRollbackResult{}, fmt.Errorf("codex: thread/rollback: %w", err)
	}
	result, err := parseThreadRollbackResponse(resp)
	if err != nil {
		return ThreadRollbackResult{}, fmt.Errorf("codex: thread/rollback: %w", err)
	}
	if result.ThreadID != threadID {
		return ThreadRollbackResult{}, fmt.Errorf("codex: thread/rollback: response thread id %q does not match target %q", result.ThreadID, threadID)
	}
	return result, nil
}

func parseThreadRollbackResponse(data json.RawMessage) (ThreadRollbackResult, error) {
	var response struct {
		Thread struct {
			ID    string     `json:"id"`
			Turns []struct{} `json:"turns"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return ThreadRollbackResult{}, fmt.Errorf("decode response: %w", err)
	}
	if response.Thread.ID == "" {
		return ThreadRollbackResult{}, fmt.Errorf("response missing thread.id")
	}
	if response.Thread.Turns == nil {
		return ThreadRollbackResult{}, fmt.Errorf("response missing thread.turns")
	}
	return ThreadRollbackResult{
		ThreadID:  response.Thread.ID,
		TurnCount: len(response.Thread.Turns),
	}, nil
}

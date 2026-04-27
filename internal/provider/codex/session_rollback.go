package codex

import (
	"context"
	"fmt"
)

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
// /Users/randy/repos/codex-source/codex-rs/app-server-protocol/schema/typescript/v2/ThreadRollbackParams.ts
func (s *Session) Rollback(ctx context.Context, numTurns int) error {
	return s.RollbackThread(ctx, s.codexThreadID, numTurns)
}

// RollbackThread is like Rollback but targets an explicit threadID rather
// than the session's own bound thread. Required for fork-at-point: after
// `thread/fork` returns a new forkID, the caller issues
// `thread/rollback(forkID, N)` over the SAME stdio session — the
// app-server routes per-threadID via its in-memory thread_manager
// (verified by spike at /tmp/spike-codex-fork/), so this writes the
// `ThreadRolledBack` marker into the FORK's rollout, not the source's.
func (s *Session) RollbackThread(ctx context.Context, threadID string, numTurns int) error {
	if threadID == "" {
		return fmt.Errorf("codex: thread/rollback: threadID is empty")
	}
	if numTurns < 1 {
		return fmt.Errorf("codex: thread/rollback: numTurns must be >= 1, got %d", numTurns)
	}
	if _, err := s.sendRequest(ctx, "thread/rollback", map[string]any{
		"threadId": threadID,
		"numTurns": numTurns,
	}); err != nil {
		return fmt.Errorf("codex: thread/rollback: %w", err)
	}
	return nil
}

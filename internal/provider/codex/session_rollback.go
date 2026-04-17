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
	if numTurns < 1 {
		return fmt.Errorf("codex: thread/rollback: numTurns must be >= 1, got %d", numTurns)
	}
	if _, err := s.sendRequest(ctx, "thread/rollback", map[string]any{
		"threadId": s.codexThreadID,
		"numTurns": numTurns,
	}); err != nil {
		return fmt.Errorf("codex: thread/rollback: %w", err)
	}
	return nil
}

package codex

import (
	"context"
	"fmt"
)

// disableResumedThreadMemories stores a resumed thread's memory mode as
// disabled when Config.NativeMemoryOff is set. `memories.generate_memories`
// decides the mode a thread is created with; a resume keeps the stored mode
// (codex-rs thread-store ThreadMetadataSync::for_resume), so a thread created
// while Codex memories were on would still be mined. The mode stays disabled
// after the setting is turned back on, as it does for a thread created while
// it was off.
func (s *Session) disableResumedThreadMemories(ctx context.Context) error {
	if !s.nativeMemoryOff {
		return nil
	}
	if _, err := s.sendRequest(ctx, "thread/memoryMode/set", map[string]any{
		"threadId": s.rootThreadID(),
		"mode":     "disabled",
	}); err != nil {
		return fmt.Errorf("codex: thread/memoryMode/set: %w", err)
	}
	return nil
}

// reportResumedThreadMemoriesFailure tells the user the thread may still be
// mined. The session stays usable, and the next resume tries again.
func (s *Session) reportResumedThreadMemoriesFailure(err error) {
	if err == nil {
		return
	}
	s.emitNonFatalError(fmt.Sprintf(
		"Agent Overflow could not exclude this Codex thread from Codex memories, so Codex can still learn from it; its next session start tries again: %v", err))
}

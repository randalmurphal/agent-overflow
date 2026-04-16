package codex

import (
	"context"
	"fmt"
)

// Fork asks the Codex app-server to create a provider-side fork of the active
// thread and returns the new provider thread ID.
func (s *Session) Fork(ctx context.Context) (string, error) {
	resp, err := s.sendRequest(ctx, "thread/fork", map[string]any{
		"threadId": s.codexThreadID,
	})
	if err != nil {
		return "", fmt.Errorf("codex: thread/fork: %w", err)
	}

	forkedThreadID := readNestedString(resp, "thread", "id")
	if forkedThreadID == "" {
		forkedThreadID = readTopLevelString(resp, "threadId")
	}
	if forkedThreadID == "" {
		return "", fmt.Errorf("codex: thread/fork: response did not contain a thread ID")
	}

	return forkedThreadID, nil
}

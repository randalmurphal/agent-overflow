package codex

import (
	"context"
	"encoding/json"
	"fmt"
)

// Fork asks the Codex app-server to create a full-history fork of the
// active thread and returns the new provider thread ID.
func (s *Session) Fork(ctx context.Context) (string, error) {
	return s.ForkAt(ctx, "")
}

// ForkAt forks the active thread truncated at lastTurnID — inclusive:
// the fork contains that turn and omits everything after it. An empty
// lastTurnID forks the full history. The source thread is unchanged;
// callers point their resume cursor at the returned fork ID.
//
// This is the replacement for the deprecated `thread/rollback` flow.
// The `lastTurnId` param exists from Codex 0.143.0 (enforced by
// provider.minimumCodexCLIVersion); an unknown turn id fails the RPC
// loudly rather than silently full-forking. Both behaviors, the
// camelCase param shape, and the inclusive-cut semantics are
// spike-verified against codex-cli 0.144
// (scratchpad spike-fork-at-turn, 2026-07-10).
//
// Turn granularity is an app-server API limit, not a core one
// (source-verified at rust-v0.144.5 and rust-v0.145.0-alpha.23,
// 2026-07-17): the fork handler's only cut is
// truncate_rollout_after_turn_id, while codex-rs core already owns a
// message-granular cut — ForkSnapshot::TruncateBeforeNthUserMessage
// slices the rollout strictly before the nth user message, mid-turn
// steers included. That granularity is what the Codex TUI's own
// Esc-Esc backtrack uses, but it reaches it through `thread/rollback`
// (num_turns counts user-MESSAGE boundaries, not wire turns), which
// answers external callers with "thread/rollback is deprecated and
// will be removed soon" and mutates the source thread in place —
// neither fits our fork-and-repoint revert model. When thread/fork
// grows a message-granular anchor, add a ForkAt variant for it and
// switch the revert/fork callers so Codex reverts gain the same
// mid-turn precision Claude's session-file slice already has (see the
// truncation-granularity comment in revertConversationLocked).
//
// The response's surviving turn tail is validated against the
// requested anchor: a mismatch would mean the fork kept turns we asked
// to drop (or vice versa), and building local truncation on top of
// that is worse than failing the whole operation.
func (s *Session) ForkAt(ctx context.Context, lastTurnID string) (string, error) {
	params := map[string]any{"threadId": s.rootThreadID()}
	if lastTurnID != "" {
		params["lastTurnId"] = lastTurnID
	}
	resp, err := s.sendRequest(ctx, "thread/fork", params)
	if err != nil {
		return "", fmt.Errorf("codex: thread/fork: %w", err)
	}
	forked, err := parseThreadForkResponse(resp)
	if err != nil {
		return "", fmt.Errorf("codex: thread/fork: %w", err)
	}
	if lastTurnID != "" && forked.LastTurnID != lastTurnID {
		return "", fmt.Errorf(
			"codex: thread/fork: fork %s survives through turn %q, expected anchor %q",
			forked.ThreadID, forked.LastTurnID, lastTurnID,
		)
	}
	return forked.ThreadID, nil
}

// threadForkResult is the subset of Codex's ThreadForkResponse that
// ForkAt needs: the new thread's identity and the id of its final
// surviving turn (empty when the fork has no turns).
type threadForkResult struct {
	ThreadID   string
	LastTurnID string
}

func parseThreadForkResponse(data json.RawMessage) (threadForkResult, error) {
	var response struct {
		Thread struct {
			ID    string `json:"id"`
			Turns []struct {
				ID string `json:"id"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return threadForkResult{}, fmt.Errorf("decode response: %w", err)
	}
	if response.Thread.ID == "" {
		return threadForkResult{}, fmt.Errorf("response missing thread.id")
	}
	result := threadForkResult{ThreadID: response.Thread.ID}
	if n := len(response.Thread.Turns); n > 0 {
		result.LastTurnID = response.Thread.Turns[n-1].ID
	}
	return result, nil
}

package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
// It is also the cut every supported codex answers, which is why it is
// still here: Revert (session_revert.go) needs BOTH a 0.148 app-server
// and a paginated-history thread, so the in-place rollback paths prefer
// that one and fall back to this.
//
// Turn granularity is an app-server API limit, not a core one
// (source-verified at rust-v0.144.5 and rust-v0.145.0-alpha.23,
// 2026-07-17; re-verified at rust-v0.149.0, 2026-08-21): every cut the
// fork handler offers lands on a TURN boundary —
// truncate_rollout_after_turn_id for the inclusive `lastTurnId` AO sends,
// truncate_rollout_before_turn_id for the exclusive `beforeTurnId` added
// in 0.146.0 behind #[experimental("thread/fork.beforeTurnId")] (the two
// cannot be combined), and the in-place `thread/revert` added in 0.148.0
// (session_revert.go), which AO PREFERS for the in-place rollback paths
// and falls back from to this one. See AGENTS.md §"History truncation:
// three cuts, all turn-granular". Meanwhile codex-rs core already owns a
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
// The response deliberately excludes turns. AO owns its rendered transcript
// in SQLite, and asking Codex to serialize the provider's full history into one
// JSON-RPC response can exceed the process line cap on a long thread.
// `thread/fork.excludeTurns` and `thread/turns/list` both exist at AO's 0.143
// minimum (source-verified at rust-v0.143.0 and rust-v0.150.1), so this needs
// no capability downgrade. An anchored fork validates its surviving tail with
// one metadata-only descending page instead: a mismatch would mean the fork
// kept turns we asked to drop (or vice versa), and building local truncation on
// top of that is worse than failing the whole operation.
func (s *Session) ForkAt(ctx context.Context, lastTurnID string) (string, error) {
	// A fork is a new thread and resolves developer instructions from
	// config like any cold start, so the composed value has to ride the
	// request or the child loses it.
	return forkThreadWith(ctx, s.sendRequest, s.rootThreadID(), lastTurnID, s.developerInstructionsValue())
}

// rpcCaller is one JSON-RPC request over some app-server connection. A
// session's sendRequest and a one-shot client's request both have this shape,
// which is what lets the fork cut run over either.
type rpcCaller func(ctx context.Context, method string, params any) (json.RawMessage, error)

// ForkSpec describes a fork cut over a throwaway app-server that loads no
// thread.
type ForkSpec struct {
	// Binary is the codex CLI path. Empty falls back to "codex" on PATH.
	Binary string
	// WorkDir is the source thread's workspace: the app-server resolves
	// project configuration from its cwd.
	WorkDir string
	// Env is the per-provider environment for the spawned process.
	Env map[string]string
	// SourceThreadID is the provider thread to fork.
	SourceThreadID string
	// LastTurnID is the inclusive cut; empty forks the full history.
	LastTurnID string
}

// ForkThread cuts a fork of SourceThreadID over a threadless app-server and
// returns the new provider thread id.
//
// This is the only way a fork may be cut, live source or not. `thread/fork`
// reads its source from the thread store, so it needs neither the source
// loaded nor the source's writer, but it LOADS THE CHILD into the process
// that answered: the child's live recorder, and with it the child's
// cross-process writer lock, belong to that app-server until it exits. A
// fork issued through the source's live session would therefore leave the
// child open in the source's process, and the child's own first start would
// be refused as "open in another Codex process". The throwaway process
// here exits with the cut, which releases the child.
//
// The child's developer instructions are not composed here: its own first
// resume carries the app's composed value like every session start does.
func ForkThread(ctx context.Context, spec ForkSpec) (string, error) {
	if strings.TrimSpace(spec.SourceThreadID) == "" {
		return "", fmt.Errorf("codex: thread/fork: source thread id is required")
	}
	client, err := startOneshotClient(ctx, oneshotSpec{
		Binary:     spec.Binary,
		WorkDir:    spec.WorkDir,
		Env:        spec.Env,
		ClientName: "agent-overflow-fork",
		Label:      "thread fork",
	})
	if err != nil {
		return "", err
	}
	defer client.close()
	return forkThreadWith(ctx, client.request, spec.SourceThreadID, spec.LastTurnID, "")
}

// forkThreadWith is the cut itself: the request, the response check and,
// for an anchored fork, the tail validation, over whichever connection the
// caller holds.
func forkThreadWith(ctx context.Context, call rpcCaller, sourceThreadID, lastTurnID, developerInstructions string) (string, error) {
	params := map[string]any{
		"threadId":     sourceThreadID,
		"excludeTurns": true,
	}
	if lastTurnID != "" {
		params["lastTurnId"] = lastTurnID
	}
	if developerInstructions != "" {
		params["developerInstructions"] = developerInstructions
	}
	resp, err := call(ctx, "thread/fork", params)
	if err != nil {
		return "", fmt.Errorf("codex: thread/fork: %w", classifyThreadWriterConflict(err))
	}
	forked, err := parseThreadForkResponse(resp)
	if err != nil {
		return "", fmt.Errorf("codex: thread/fork: %w", err)
	}
	if lastTurnID != "" {
		newestTurnID, err := newestThreadTurnIDWith(ctx, call, forked.ThreadID)
		if err != nil {
			return "", fmt.Errorf("codex: thread/fork: validate fork %s tail: %w", forked.ThreadID, err)
		}
		if newestTurnID != lastTurnID {
			return "", fmt.Errorf(
				"codex: thread/fork: fork %s survives through turn %q, expected anchor %q",
				forked.ThreadID, newestTurnID, lastTurnID,
			)
		}
	}
	return forked.ThreadID, nil
}

// threadForkResult is the subset of Codex's ThreadForkResponse that ForkAt
// needs. Turns are intentionally excluded from the request and validated
// separately when the fork has an anchor.
type threadForkResult struct {
	ThreadID string
}

func parseThreadForkResponse(data json.RawMessage) (threadForkResult, error) {
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return threadForkResult{}, fmt.Errorf("decode response: %w", err)
	}
	if response.Thread.ID == "" {
		return threadForkResult{}, fmt.Errorf("response missing thread.id")
	}
	return threadForkResult{ThreadID: response.Thread.ID}, nil
}

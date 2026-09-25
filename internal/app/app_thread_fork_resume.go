package app

import (
	"context"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// forkResumeState is the provider resume wiring resolveForkResumeState
// hands back for a new fork:
//
//   - SessionRef: the fork's own provider session id, set when the fork
//     materialized its transcript up front (a Claude anchored slice, a
//     Codex thread/fork child).
//   - PendingForkRef: the SOURCE session id for a lazy Claude fork —
//     the first session start passes `--fork-session --resume <ref>`.
//   - PinnedResumeAt: the transcript cut for a PINNED lazy Claude fork
//     (any tail fork): the leaf uuid captured when Fork
//     was clicked. The first session start repairs it against the CLI's
//     resume filters and passes `--resume-session-at`, so the CLI's own
//     fork cuts exactly where the timeline was cut instead of
//     wherever the source has grown to by first send. Empty on an
//     anchored fork, which already owns its sliced session file.
//   - Cleanup: undoes provider-side artifacts (a JSONL slice on disk)
//     when a later fork step fails. Codex thread/fork children cannot
//     be deleted over JSON-RPC; orphan rollouts are accepted there.
type forkResumeState struct {
	SessionRef     string
	PendingForkRef string
	PinnedResumeAt string
	Cleanup        func() error
}

// resolveForkResumeState wires the provider-specific resume reference for
// the new fork. See forkResumeState for the field contract.
//
// Every Claude tail fork supplies a cut captured before the fork's cut,
// including idle sources. This prevents later source turns from entering an unstarted
// fork's context. Codex materializes its native fork through a temporary
// app-server; an active tail uses its native interrupted-fork semantics.
func (a *App) resolveForkResumeState(ctx context.Context, source store.Thread, atTurnIndex *int, midTurnCut *claudeMidTurnCut) (forkResumeState, error) {
	switch source.Provider {
	case string(provider.Codex):
		ref, err := a.forkCodexThread(ctx, source, atTurnIndex)
		if err != nil {
			return forkResumeState{}, fmt.Errorf("fork thread: fork codex provider state: %w", err)
		}
		return forkResumeState{SessionRef: ref}, nil
	case string(provider.Claude):
		return a.forkClaudeThread(source, atTurnIndex, midTurnCut)
	default:
		return forkResumeState{}, fmt.Errorf("fork thread: unsupported provider %q", source.Provider)
	}
}

func (a *App) resolveMessageForkResumeState(ctx context.Context, source store.Thread, anchor store.MessageAnchor, anchorItem store.Item) (forkResumeState, error) {
	switch source.Provider {
	case string(provider.Codex):
		// Codex forks are turn-granular (thread/fork cuts at a turn
		// boundary), so the anchor's intra-turn position is irrelevant:
		// the whole anchor turn is dropped, matching the turn-granular
		// fork cut.
		if anchor.TurnIndex == 0 {
			return forkResumeState{}, nil
		}
		lastKeptTurn := anchor.TurnIndex - 1
		ref, err := a.forkCodexThread(ctx, source, &lastKeptTurn)
		if err != nil {
			return forkResumeState{}, fmt.Errorf("fork thread from message: fork codex provider state: %w", err)
		}
		return forkResumeState{SessionRef: ref}, nil
	case string(provider.Claude):
		return a.forkClaudeThreadBeforeMessage(source, anchor, anchorItem)
	default:
		return forkResumeState{}, fmt.Errorf("fork thread from message: unsupported provider %q", source.Provider)
	}
}

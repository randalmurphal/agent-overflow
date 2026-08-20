// Codex half of the fork saga (app_thread_fork.go): the `thread/fork`
// call over the live app-server session (or a throwaway resume session),
// anchor resolution to a provider turn id, and the in-flight-turn anchor
// guard.
package main

import (
	"context"
	"fmt"
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
)

// forkCodexThread creates a new Codex thread that mirrors the source up
// to atTurnIndex (or the tail when atTurnIndex == nil). One
// `thread/fork` with the resolved lastTurnId anchor does the whole cut
// (Codex >= 0.143); the source thread is untouched. Returns "" (no
// error) when the kept prefix has no provider-backed turns at all —
// the fork then starts a fresh provider thread on its first send, the
// same contract as resolveMessageForkResumeState's turn-0 branch.
//
// atTurnIndex == nil mid-turn is the tail fork's mid-turn shape and
// needs no special case: `thread/fork` with no lastTurnId is what codex
// documents for "snapshot as if interrupted" (0.147.0). An ANCHORED
// mid-turn fork must stay strictly below the in-flight turn — codex
// REJECTS a lastTurnId naming an in-progress turn — which both callers'
// cuts already guarantee (ForkThread normalizes such an anchor to a tail
// fork; the message path anchors at `anchor.TurnIndex - 1`). The
// assertion below is what catches a future caller that stops doing it:
// `InsertTurn` stamps provider_turn_id at insert, so an OPEN turn row
// really can carry an anchor codex would refuse, and the failure mode is
// a fork whose provider history disagrees with its cloned items.
func (a *App) forkCodexThread(source store.Thread, atTurnIndex *int) (string, error) {
	const op = "fork codex thread"
	lastTurnID := ""
	if atTurnIndex != nil {
		activeTurn, active, err := a.store.GetActiveTurn(source.ID)
		if err != nil {
			return "", fmt.Errorf("%s: %w", op, err)
		}
		if active && *atTurnIndex >= activeTurn.TurnIndex {
			return "", fmt.Errorf(
				"%s: anchor turn %d is at or above thread %s's in-flight turn %d — codex rejects a lastTurnId naming an in-progress turn; fork at the tail instead",
				op, *atTurnIndex, source.ID, activeTurn.TurnIndex,
			)
		}
		anchor, found, err := a.resolveCodexForkAnchor(source.ID, *atTurnIndex)
		if err != nil {
			return "", fmt.Errorf("%s: %w", op, err)
		}
		if !found {
			log.Printf("%s: thread %s has no provider-backed turns at or before %d — fork starts a fresh provider thread", op, source.ID, *atTurnIndex)
			return "", nil
		}
		lastTurnID = anchor
	}
	// Required only once a provider fork is actually happening: an
	// anchored fork of a local-only prefix returned fresh above, so
	// reaching here with no thread reference is an inconsistent row
	// (tail fork of a never-connected thread, or provider-backed turns
	// on a thread that lost its ref).
	if source.SessionRef == "" {
		return "", fmt.Errorf("%s: source thread %q is missing a Codex thread reference", op, source.ID)
	}
	forkedID, err := a.forkCodexThreadAt(source, lastTurnID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	return forkedID, nil
}

// resolveCodexForkAnchor picks the `thread/fork` lastTurnId for a cut
// that keeps turns <= lastKeptTurnIndex: the provider turn id of the
// LATEST provider-backed turn at or before that index. Walking down
// (instead of taking lastKeptTurnIndex verbatim) skips AO turn indexes
// that never became provider turns — failed sends that errored before
// the wire, and any turn whose row predates provider-id stamping.
//
// (found=false, err=nil) means the kept prefix holds no provider-backed
// turns at all; the caller starts a fresh provider thread. That answer
// is only trusted when the ITEMS agree — a prefix that carries
// provider-confirmed user messages but no provider turn ids is a
// legacy-data hole (a fork cloned before turns rows were copied), and
// silently discarding its provider history would be worse than failing.
func (a *App) resolveCodexForkAnchor(threadID string, lastKeptTurnIndex int) (string, bool, error) {
	for idx := lastKeptTurnIndex; idx >= 0; idx-- {
		turn, found, err := a.store.GetTurnByThreadIndex(threadID, idx)
		if err != nil {
			return "", false, fmt.Errorf("resolve codex fork anchor: %w", err)
		}
		if found && turn.ProviderTurnID != "" {
			return turn.ProviderTurnID, true, nil
		}
	}
	providerBacked, err := a.knownCodexProviderTurnCountBefore(threadID, lastKeptTurnIndex+1)
	if err != nil {
		return "", false, fmt.Errorf("resolve codex fork anchor: %w", err)
	}
	if providerBacked > 0 {
		return "", false, fmt.Errorf(
			"resolve codex fork anchor: thread %s has %d provider-backed turns at or before %d but no recorded provider turn id — likely a fork created before turn rows were cloned; fork the thread again from the desired message",
			threadID, providerBacked, lastKeptTurnIndex,
		)
	}
	return "", false, nil
}

// forkCodexThreadAt issues `thread/fork` (cut at lastTurnID, or full
// history when "") through the thread's live app-server session, or a
// throwaway resume session when none is active.
func (a *App) forkCodexThreadAt(source store.Thread, lastTurnID string) (string, error) {
	if activeSession, ok := a.activeCodexSession(source.ID); ok {
		return activeSession.ForkAt(context.Background(), lastTurnID)
	}

	tempSession, err := codex.NewSession(context.Background(), source.ID, codex.Config{
		Binary:         a.providerBinaryPath(source.Provider),
		Model:          source.Model,
		WorkDir:        source.WorkspacePath,
		ResumeThreadID: source.SessionRef,
		// Boot-mode overrides only, deliberately no `ao` credential: this is a
		// throwaway app-server used to issue one fork request, not a session an
		// agent takes a turn in.
		Env:         a.sessionProcessEnv(source.Provider, nil, aoSessionCredential{}),
		EventLogger: a.logger,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		return "", fmt.Errorf("resume source thread: %w", err)
	}
	defer tempSession.Close()

	return tempSession.ForkAt(context.Background(), lastTurnID)
}

func (a *App) activeCodexSession(threadID string) (*codex.Session, bool) {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.codex == nil {
		return nil, false
	}
	return sess.codex, true
}

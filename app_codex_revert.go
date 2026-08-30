// Codex history truncation for the in-place rollback paths
// (app_conversation_rollback.go): choosing between upstream's two usable
// cuts, and resolving the exclusive anchor `thread/revert` needs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
)

// codexHistoryCut is the outcome of truncating a Codex thread's provider
// history for an in-place rollback.
//
// Two shapes, and the difference is thread identity:
//
//   - Reverted: `thread/revert` cut the SAME provider thread. SessionRef
//     is unchanged and the next send resumes the thread the user has been
//     talking to all along.
//   - Forked: `thread/fork` produced a new provider thread carrying the
//     kept prefix, and SessionRef must be repointed at ThreadRef.
//
// AO's OWN thread row (and therefore its id, its title, its workspace and
// its worktree) is untouched either way — both cuts are provider-side. The
// fork's cost is not a new AO thread, it is a new Codex thread id, a fresh
// rollout file, and the loss of provider-side thread continuity for
// anything keyed on it (upstream thread usage/cost estimates, `codex
// resume` from a terminal, a TUI that had the thread open).
type codexHistoryCut struct {
	ThreadRef string
	Reverted  bool
}

// cutCodexThreadHistory truncates a Codex thread's provider history so it
// keeps everything through lastKeptTurnID and drops firstDroppedTurnID
// onwards, preferring the in-place cut.
//
// The two anchors describe the SAME boundary from opposite sides —
// `thread/fork` takes the last KEPT turn (inclusive), `thread/revert` the
// first DROPPED one (exclusive) — and they are resolved separately
// because AO's turn rows can be missing either neighbour. An empty
// firstDroppedTurnID means "no provider-backed turn is being dropped that
// AO can name", which is not an error: the fork cut needs no such anchor,
// so the call simply forks.
//
// Both cuts run inside ONE app-server connection (withCodexThreadSession),
// which is what keeps the version/history-mode probe free: the answer is
// already on the session that would have forked anyway.
//
// Falling back to the fork after a REFUSED revert is only safe because
// every refusal that reaches ErrThreadRevertUnsupported is raised before
// upstream changes durable history (see codex.classifyThreadRevertError). Any
// revert error that is neither a pre-history-mutation refusal nor an UNRESOLVED
// outcome aborts the whole truncation — a fork built on a half-reverted
// thread would silently disagree with both.
//
// The two unresolved shapes are what make a RETRY converge, and they are
// resolved the same way: by asking the provider whether the boundary is
// already in place (VerifyRevertBoundary), never by assuming.
//
//   - ErrThreadRevertAnchorUnresolvable. The provider cut and the local
//     row deletion cannot be one transaction: when the provider half
//     succeeds and the local half then fails, AO's rows still name the
//     turn upstream has just dropped, so the user's next attempt asks to
//     revert at an anchor that no longer exists. Treated as a hard error,
//     that thread could never be edited again — every retry re-raises the
//     same refusal.
//   - ErrThreadRevertOutcomeUnknown. The request was written and nothing
//     answered it; upstream mutates before it replies, so this says
//     nothing about whether the cut landed.
//
// A verified boundary converges IN PLACE — the retry finishes on the same
// provider thread the user has been talking to. An unverified one falls
// back to the fork, which converges on the SAME history either way: the
// fork's anchor is the last KEPT turn, which survives in the retained
// prefix whether or not the earlier revert landed. The cost is the one
// the fork always carries — a new provider thread id — which is the right
// price for a thread that would otherwise be stuck.
func (a *App) cutCodexThreadHistory(source store.Thread, lastKeptTurnID, firstDroppedTurnID string) (codexHistoryCut, error) {
	cut := codexHistoryCut{}
	// The pre-resume hook cannot return: it is also codex.Config.BeforeResume,
	// which the session constructor calls with nowhere to report to. So its
	// verdict is carried out in a variable and re-raised as fn's first act,
	// which is the earliest point that can still refuse — before the revert,
	// before the fork, and before provider history changes.
	var purgeErr error
	err := a.withCodexThreadSessionPreparedBy(source, func(session *codex.Session) {
		// The provider's own message queue, over whichever connection this
		// bracket produced, and BEFORE the resume that would load the thread.
		// The rollback purges through a LIVE session before stopping it; a
		// thread that had none reaches its queue for the first time here, and
		// an in-place revert keeps the thread id, so a row left behind re-runs
		// a rolled-back message on the next resume. Loading the thread is what
		// arms its idle hook, so on the throwaway connection this is the only
		// point where the rows can be dropped without the drain racing the cut.
		// No-op on a pre-0.148 app-server, and no-op again when the live purge
		// already emptied it.
		purgeErr = a.purgeCodexQueueBeforeCut(source.ID, session)
	}, func(session *codex.Session) error {
		// A queue this connection could not empty is a set of messages armed
		// to run against the history about to be cut away. Cutting anyway
		// would deliver them minutes or days later onto a thread that no
		// longer contains what they answered.
		if purgeErr != nil {
			return purgeErr
		}
		revertCut, needsFork, revertErr := a.tryCodexThreadRevert(
			session, source.ID, lastKeptTurnID, firstDroppedTurnID,
		)
		if revertErr != nil {
			return revertErr
		}
		if !needsFork {
			cut = revertCut
			return nil
		}
		forkedID, err := session.ForkAt(context.Background(), lastKeptTurnID)
		if err != nil {
			return err
		}
		cut = codexHistoryCut{ThreadRef: forkedID}
		return nil
	})
	if err != nil {
		return codexHistoryCut{}, err
	}
	return cut, nil
}

// tryCodexThreadRevert attempts the same-thread cut without silently changing
// thread identity. needsFork means the caller may use the inclusive fork
// anchor after applying whatever session-lifecycle boundary its caller needs.
// A live active-turn caller stops the session before that fallback; a cold
// throwaway session may fork on the connection it already owns.
func (a *App) tryCodexThreadRevert(
	session *codex.Session,
	threadID, lastKeptTurnID, firstDroppedTurnID string,
) (cut codexHistoryCut, needsFork bool, err error) {
	if firstDroppedTurnID == "" || !session.SupportsThreadRevert() {
		return codexHistoryCut{}, true, nil
	}

	reverted, err := session.Revert(context.Background(), firstDroppedTurnID)
	switch {
	case err == nil:
		return codexHistoryCut{ThreadRef: reverted.ThreadID, Reverted: true}, false, nil
	case errors.Is(err, codex.ErrThreadRevertUnsupported):
		// Version skew between what the thread reported and what this
		// app-server will do. Loud, because the pre-flight gate is supposed
		// to make this unreachable.
		log.Printf("app: codex rollback: thread %s refused the in-place cut (%v); falling back to thread/fork", threadID, err)
		return codexHistoryCut{}, true, nil
	case errors.Is(err, codex.ErrThreadRevertAnchorUnresolvable),
		errors.Is(err, codex.ErrThreadRevertOutcomeUnknown):
		// Neither shape says the thread was left alone, and both ask the
		// provider the same question: is the boundary already here? An
		// applied boundary converges in place. Otherwise the inclusive fork
		// anchor lands on the same retained history.
		log.Printf("app: codex rollback: thread %s left the in-place cut at turn %s unresolved (%v); verifying the provider boundary", threadID, firstDroppedTurnID, err)
		if verified, applied := a.codexRevertBoundaryApplied(session, threadID, lastKeptTurnID, firstDroppedTurnID); applied {
			log.Printf("app: codex rollback: thread %s is already cut at turn %s; converging in place", threadID, firstDroppedTurnID)
			return codexHistoryCut{ThreadRef: verified.ThreadID, Reverted: true}, false, nil
		}
		return codexHistoryCut{}, true, nil
	case errors.Is(err, codex.ErrThreadRevertInFlight):
		return codexHistoryCut{}, false, fmt.Errorf("another rollback is still cutting this thread's history; try again in a moment: %w", err)
	default:
		return codexHistoryCut{}, false, err
	}
}

// forkCodexThreadHistory performs only the identity-changing fallback. It is
// separate from cutCodexThreadHistory so a live mid-turn revert refusal can
// stop that runtime before opening the cold fork connection, without issuing
// thread/revert a second time.
func (a *App) forkCodexThreadHistory(source store.Thread, lastKeptTurnID string) (codexHistoryCut, error) {
	cut := codexHistoryCut{}
	var purgeErr error
	err := a.withCodexThreadSessionPreparedBy(source, func(session *codex.Session) {
		purgeErr = a.purgeCodexQueueBeforeCut(source.ID, session)
	}, func(session *codex.Session) error {
		if purgeErr != nil {
			return purgeErr
		}
		forkedID, err := session.ForkAt(context.Background(), lastKeptTurnID)
		if err != nil {
			return err
		}
		cut = codexHistoryCut{ThreadRef: forkedID}
		return nil
	})
	if err != nil {
		return codexHistoryCut{}, err
	}
	return cut, nil
}

// codexRevertBoundaryApplied asks the provider whether the boundary an
// unresolved cut aimed at is already this thread's boundary.
//
// The probe is read-only and its failure is never fatal here: a caller
// that cannot verify falls back to the fork, which converges on the same
// history either way (its anchor is the last KEPT turn, retained in both
// worlds). Reporting "not applied" on a probe error is therefore the
// conservative answer, not a guess — it costs a provider thread id, never
// correctness.
func (a *App) codexRevertBoundaryApplied(session *codex.Session, threadID, lastKeptTurnID, firstDroppedTurnID string) (codex.RevertedThread, bool) {
	verified, applied, err := session.VerifyRevertBoundary(context.Background(), lastKeptTurnID, firstDroppedTurnID)
	if err != nil {
		log.Printf("app: codex rollback: thread %s could not verify the cut boundary at turn %s (%v); falling back to thread/fork", threadID, firstDroppedTurnID, err)
		return codex.RevertedThread{}, false
	}
	return verified, applied
}

// resolveCodexRevertAnchor picks the `thread/revert` beforeTurnId for a
// cut that drops turns >= firstDroppedTurnIndex: the provider turn id of
// the EARLIEST provider-backed turn at or after that index.
//
// The mirror image of resolveCodexForkAnchor, which walks DOWN for the
// last surviving turn. Walking up (instead of taking
// firstDroppedTurnIndex verbatim) skips AO turn indexes that never became
// provider turns — a send that failed before the wire leaves a turn row
// with no provider id, and naming it would be an anchor upstream cannot
// resolve.
//
// (found=false, err=nil) means no turn being dropped is one AO can name
// on the wire. That is NOT a hole to fail on: the caller falls back to
// the fork cut, whose inclusive anchor describes the same boundary from
// the surviving side and whose own resolver already refuses a prefix with
// provider-backed items but no turn ids.
//
// Known divergence from the fork cut: a provider turn that exists
// BETWEEN the two anchors without an AO turn row (a cut made by something
// other than a user send) is dropped by the fork's inclusive cut and kept
// by the revert's exclusive one. AO's turn rows cover every user send and
// every adopted external turn, so this needs a provider-side turn AO
// never observed at all.
func (a *App) resolveCodexRevertAnchor(threadID string, firstDroppedTurnIndex int) (string, bool, error) {
	if firstDroppedTurnIndex < 0 {
		firstDroppedTurnIndex = 0
	}
	// One query for the ceiling instead of probing upward until the gaps
	// look convincing: turn indexes can skip (a failed send never opens a
	// row), so "the first miss ends the walk" would stop early.
	newest, err := a.store.ListRecentTurns(threadID, 1)
	if err != nil {
		return "", false, fmt.Errorf("resolve codex revert anchor: %w", err)
	}
	if len(newest) == 0 {
		return "", false, nil
	}
	for idx := firstDroppedTurnIndex; idx <= newest[0].TurnIndex; idx++ {
		turn, found, err := a.store.GetTurnByThreadIndex(threadID, idx)
		if err != nil {
			return "", false, fmt.Errorf("resolve codex revert anchor: %w", err)
		}
		if found && turn.ProviderTurnID != "" {
			return turn.ProviderTurnID, true, nil
		}
	}
	return "", false, nil
}

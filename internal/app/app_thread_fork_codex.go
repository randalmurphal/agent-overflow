// Codex half of the fork saga (app_thread_fork.go): the `thread/fork`
// cut over a throwaway threadless app-server, anchor resolution to a
// provider turn id, and the in-flight-turn anchor guard.
package app

import (
	"context"
	"fmt"
	"log"
	"time"

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
func (a *App) forkCodexThread(ctx context.Context, source store.Thread, atTurnIndex *int) (string, error) {
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
	forkedID, err := a.forkCodexThreadAt(ctx, source, lastTurnID)
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
	return a.threadApplication().ResolveCodexForkAnchor(
		threadID, lastKeptTurnIndex, a.knownCodexProviderTurnCountBefore,
	)
}

// codexForkTimeout bounds one `thread/fork`: the app-server spawn, its
// initialize, the cut and the anchored cut's tail read. The whole fork runs
// under the SOURCE thread's action lock, so an app-server that accepts the
// connection and never answers would otherwise wedge that thread for as
// long as the process lives. It sits well above a cold spawn and well below
// any wait a person would accept on a thread they can no longer use. A var
// so tests can drive the timeout path without sleeping; never reassigned in
// production code.
var codexForkTimeout = 60 * time.Second

// forkCodexThreadAt issues `thread/fork` (cut at lastTurnID, or full
// history when "") over a throwaway app-server that loads no thread. Never
// through the source's live session, even when one is running: the cut
// loads the child into the answering process, so a live source would keep
// the child's writer and the child's own first start would be refused
// (codex.ForkThread). The source is read from the thread store, which a
// live source's recorder flushes on every write.
func (a *App) forkCodexThreadAt(ctx context.Context, source store.Thread, lastTurnID string) (string, error) {
	cut, cancel := context.WithTimeout(ctx, codexForkTimeout)
	defer cancel()
	return codex.ForkThread(cut, codex.ForkSpec{
		Binary:         a.providerBinaryPath(source.Provider),
		WorkDir:        source.WorkspacePath,
		Env:            a.sessionProcessEnv(source.Provider, nil, aoSessionCredential{}),
		SourceThreadID: source.SessionRef,
		LastTurnID:     lastTurnID,
	})
}

// withCodexThreadSessionPreparedBy runs fn against an app-server connection
// for source: the thread's live session when one is running, otherwise a
// throwaway resume session closed on the way out. The in-place history cut
// (`thread/revert`, app_codex_revert.go) runs here: a live paginated
// rollback stays on its existing session so thread/revert can own
// active-turn shutdown, and the cold path resumes the thread once to cut
// it. The fork cut does not: it must not load its child into a session
// that outlives it (forkCodexThreadAt).
//
// prepare is a hook that runs against the connection BEFORE the thread is
// loaded.
//
// The distinction only exists on the throwaway branch, and it is the whole
// point of the hook: `thread/resume` LOADS the thread, and a loaded thread's
// idle hook drains its provider-side queue. Work that must happen before a
// queued row can dispatch — the rollback's purge of rows the user just removed
// — has to run on this side of the resume, not as fn's first statement. On the
// LIVE branch there is no resume to be ahead of, so prepare simply runs first;
// the thread has been loaded for as long as the session has existed and the
// purge is racing the idle hook either way.
func (a *App) withCodexThreadSessionPreparedBy(
	source store.Thread, prepare func(*codex.Session), fn func(*codex.Session) error,
) error {
	if activeSession, ok := a.activeCodexSession(source.ID); ok {
		if prepare != nil {
			prepare(activeSession)
		}
		return fn(activeSession)
	}

	tempSession, err := codex.NewSession(context.Background(), source.ID, codex.Config{
		Binary:         a.providerBinaryPath(source.Provider),
		Model:          source.Model,
		WorkDir:        source.WorkspacePath,
		ResumeThreadID: source.SessionRef,
		BeforeResume:   prepare,
		// Boot-mode overrides only, deliberately no `ao` credential: this is a
		// throwaway app-server used to issue one fork request, not a session an
		// agent takes a turn in.
		Env:         a.sessionProcessEnv(source.Provider, nil, aoSessionCredential{}),
		EventLogger: a.logger,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		return fmt.Errorf("resume source thread: %w", err)
	}
	defer tempSession.Close()

	return fn(tempSession)
}

func (a *App) activeCodexSession(threadID string) (*codex.Session, bool) {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.Codex == nil {
		return nil, false
	}
	return sess.Codex, true
}

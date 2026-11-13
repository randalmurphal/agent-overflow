// A person's Stop (the Stop button, Esc, and declining an approval or
// user-input prompt). The sent message stays: the turn is interrupted, and
// the working presentation clears in the press's tick (Claude Code's
// `resetLoadingState`, the Codex TUI's spinner clear on `TurnAborted`); the
// real `provider:turn_completed` re-runs the same projection.

import type { ErrorSurface, PaneSession } from './threadPaneRoles';
import { errString } from '../utils/errors';
import { userFacingError } from '../utils/userFacingError';
import { isAlreadyHandled } from '../transport/alreadyHandled';
import {
  isThreadWorking,
  projectStopDelivered,
  projectTurnStopRequested,
  restoreRefusedTurnStop,
  settleTurnStopRequest,
  type ActiveTurn,
} from './threadStatuses.svelte';
import { beginThreadInterrupt, finishThreadInterrupt, isThreadInterruptCurrent } from './threadInterruptState.svelte';
import { InterruptTurn } from './bindings';
import { refusedBackgroundAgents, type BackgroundKillAgent } from '../transport/backgroundKillRefusal';
import { confirmBackgroundKill } from './backgroundKillConfirmation.svelte';
import { hasLiveSubagentRunStates } from './subagentRunState.svelte';

/**
 * What a Stop reads off a pane. Callers pass a whole `ThreadPane`; the type
 * names the slice actually used, so a new member use here fails to compile
 * until threadPaneRoles.ts lists it.
 */
type InterruptPane = Pick<ErrorSurface, 'threadId' | 'setGeneralError'> & Pick<PaneSession, 'setSendInFlight'>;

/**
 * Errors treated as no-ops on the interrupt path:
 *
 *  1. "no active turn": the turn ended before the interrupt reached it.
 *  2. The request is no longer open: a race between an explicit cancel and
 *     the CLI's own `control_cancel_request`, or another client that
 *     answered the same prompt first. The backend answers both with the
 *     `already_handled` code.
 *
 * Case 2 is checked by code first and by text second. The transport redacts
 * method-error text for every caller that is not on loopback, so only the
 * code survives for a remote client. The text arm covers loopback paths that
 * still carry a raw message and errors raised client-side, which carry no
 * code at all.
 */
export function isBenignInterruptError(err: unknown): boolean {
  if (isAlreadyHandled(err)) return true;
  const message = errString(err).toLowerCase();
  return (
    message.includes('no active turn') ||
    message.includes('already resolved') ||
    message.includes('stale interactive request')
  );
}

/**
 * Standard handler for the fire-and-forget interrupt RPCs: benign races are
 * dropped, everything else lands on the pane banner so a real provider
 * failure is not swallowed by the optimistic clear.
 */
export function reportNonBenignInterruptError(
  pane: Pick<ErrorSurface, 'setGeneralError'>,
  err: unknown,
): void {
  if (isBenignInterruptError(err)) return;
  console.error('Failed to interrupt turn:', err);
  pane.setGeneralError(userFacingError(err), err);
}

/**
 * How this client's direct send ended: the backend took the message, the
 * backend answered with an error (nothing was sent), or the reply was lost
 * and the message may be with the agent.
 */
export type DirectSendOutcome = 'accepted' | 'refused' | 'unknown';

// This client's direct sends whose SendMessage call has not answered, by
// thread.
const unansweredSends = new Map<string, Promise<DirectSendOutcome>>();

/**
 * Register this client's direct send of `threadId`. Returns how the send
 * path reports its outcome; the first call wins.
 */
export function trackDirectSend(threadId: string): (outcome: DirectSendOutcome) => void {
  let settle!: (outcome: DirectSendOutcome) => void;
  const outcome = new Promise<DirectSendOutcome>((resolve) => { settle = resolve; });
  unansweredSends.set(threadId, outcome);
  void outcome.then(() => {
    if (unansweredSends.get(threadId) === outcome) unansweredSends.delete(threadId);
  });
  return settle;
}

export function resetDirectSendsForTest(): void {
  unansweredSends.clear();
}

/**
 * Stop the thread's turn. A second Stop while one is in flight is a no-op.
 *
 * With no unanswered send of this client the Stop interrupts in the press's
 * tick. Otherwise it holds the thread's interrupt transaction (Send stays
 * gated) and leaves the working presentation up until the send answers.
 * Waiting for the answer is enough: the backend serializes a thread's send
 * and interrupt, so an interrupt issued earlier could only run before the
 * send, where there is nothing to stop. It then interrupts, unless the send
 * was refused (or never issued), which leaves nothing to stop and the send's
 * own failure path to restore the draft, or the thread's history was
 * invalidated meanwhile, which drops the transaction.
 */
export function runInterrupt(pane: InterruptPane): void {
  const threadId = pane.threadId;
  if (!threadId) return;
  const interruptToken = beginThreadInterrupt(threadId);
  if (interruptToken === null) return;
  const send = unansweredSends.get(threadId);
  if (!send) {
    void runPlainInterrupt(pane, threadId, interruptToken);
    return;
  }
  void send.then((outcome) => {
    if (!isThreadInterruptCurrent(threadId, interruptToken)) return;
    if (outcome === 'refused') finishThreadInterrupt(threadId, interruptToken);
    else void runPlainInterrupt(pane, threadId, interruptToken);
  });
}

/**
 * The Stop's optimistic clear. Returns the handle the answer to its
 * interrupt RPC takes: a refusal puts the turn back (restoreRefusedTurnStop),
 * anything else settles it.
 */
function clearWorkingPresentation(pane: InterruptPane, threadId: string): ActiveTurn | null {
  const stopped = projectTurnStopRequested(threadId);
  if (pane.threadId === threadId) pane.setSendInFlight(false);
  return stopped;
}

/**
 * InterruptTurn for a Stop whose optimistic clear is `stopped`, confirmed
 * for the agents in `confirmed` (none for an unconfirmed Stop).
 */
async function interruptTurnForStop(
  pane: InterruptPane,
  threadId: string,
  interruptToken: number,
  confirmed: readonly BackgroundKillAgent[],
  stopped: ActiveTurn | null,
): Promise<BackgroundKillAgent[] | null> {
  try {
    await InterruptTurn(threadId, confirmedAgentIds(confirmed));
    // A history invalidation during the call dropped the transaction: what
    // the thread shows now is not what this Stop stopped.
    if (isThreadInterruptCurrent(threadId, interruptToken)) projectStopDelivered(threadId);
  } catch (err) {
    const agents = refusedBackgroundAgents(err);
    if (agents !== null) return agents;
    reportNonBenignInterruptError(pane, err);
  }
  settleTurnStopRequest(threadId, stopped);
  return null;
}

/**
 * A plain interrupt. A Claude interrupt kills every live async agent the
 * session holds, so the backend refuses with `background_agents_running`
 * and the agents until the caller confirms
 * (transport/backgroundKillRefusal.ts); the person answers through the
 * app-root dialog, and "keep them" leaves the turn running. The working
 * presentation is cleared in the press's tick when no listed agent is
 * live, else after the confirmation. The backend, not the registry, is the
 * authority: a refusal on a stale registry (an agent launched since the
 * tray's last read) puts the cleared turn back before asking.
 */
async function runPlainInterrupt(
  pane: InterruptPane,
  threadId: string,
  interruptToken: number,
): Promise<void> {
  const stopped = hasLiveSubagentRunStates(threadId) ? null : clearWorkingPresentation(pane, threadId);
  let agents: BackgroundKillAgent[] | null = null;
  try {
    agents = await interruptTurnForStop(pane, threadId, interruptToken, [], stopped);
    if (agents !== null) restoreRefusedTurnStop(threadId, stopped);
  } finally {
    finishThreadInterrupt(threadId, interruptToken);
  }
  if (agents !== null) await interruptAfterConfirmation(pane, threadId, agents);
}

/** The ids a confirmation names its agents by: each one's transcript root. */
function confirmedAgentIds(agents: readonly BackgroundKillAgent[]): string[] {
  return agents.map((agent) => agent.transcriptRootId);
}

/**
 * The refused Stop's second half: ask, and on "stop everything" run a Stop
 * of its own confirmed for the agents the question named. The refused
 * Stop's transaction is already closed: the question is the person's, not
 * an interrupt in flight, so the composer keeps showing the running turn
 * and its Stop behind the dialog. Nothing is asked when the turn ended
 * before the refusal landed: there is nothing left to stop.
 *
 * An agent launched while the person read the question is not covered, so
 * the confirmed Stop is refused again naming every live agent, and the
 * person is asked again with that list. A refusal whose list cannot be
 * read names nothing a confirmation could cover: it is reported, and the
 * turn keeps running.
 */
async function interruptAfterConfirmation(
  pane: InterruptPane,
  threadId: string,
  agents: readonly BackgroundKillAgent[],
): Promise<void> {
  let asked = agents;
  for (;;) {
    if (!isThreadWorking(threadId)) return;
    const stop = await confirmBackgroundKill(threadId, asked);
    if (!stop) return;
    const interruptToken = beginThreadInterrupt(threadId);
    if (interruptToken === null) return;
    let refused: BackgroundKillAgent[] | null;
    try {
      const stopped = clearWorkingPresentation(pane, threadId);
      refused = await interruptTurnForStop(pane, threadId, interruptToken, asked, stopped);
      if (refused !== null) restoreRefusedTurnStop(threadId, stopped);
    } finally {
      finishThreadInterrupt(threadId, interruptToken);
    }
    if (refused === null) return;
    if (refused.length === 0) {
      reportNonBenignInterruptError(pane, new Error('Stop was refused for background agents the app could not read'));
      return;
    }
    asked = refused;
  }
}

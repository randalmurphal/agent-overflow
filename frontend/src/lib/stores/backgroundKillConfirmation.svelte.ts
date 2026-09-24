// Stop confirmations for the background agents a Claude interrupt would kill.
//
// A Claude interrupt kills every running or parked background agent on the
// thread, from any turn. The Stop RPCs refuse with code
// 'background_agents_running' while one is live, before they have any
// effect, and name the agents. The Stop flows in revertOnInterrupt.svelte.ts
// put the refused Stop's turn back on screen and record one pending question
// per thread here.
//
// Composer API:
//   pendingBackgroundKill(threadId)   reactive; the refused Stop awaiting an
//                                     answer, or null.
//   confirmBackgroundKill(pane, draft)
//                                     (revertOnInterrupt.svelte.ts) ends the
//                                     question and runs the Stop again, as
//                                     if pressed now, with the kill
//                                     confirmed. Returns what
//                                     runInterruptOrRevert returns.
//   dismissBackgroundKill(threadId)   ends the question; nothing is stopped.
//
// Producer API (the Stop flows):
//   backgroundKillRefusal(err)        the validated refusal, or null for any
//                                     other error.
//   offerBackgroundKillConfirmation(threadId, refusal)
//
// A question also ends when a later Stop on the thread asks again, when
// the thread's turn completes or reverts (threadStatuses.svelte.ts), and
// when the thread's live state is cleared or its history invalidated.

import { TransportError } from '../transport/wsClient';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { onThreadHistoryInvalidated } from './threadIdentityInvalidation';

export const BACKGROUND_AGENTS_RUNNING = 'background_agents_running';

export type BackgroundAgentRunState = 'running' | 'parked';

/** One agent the refused Stop would have killed (app.BackgroundKillAgent). */
export interface BackgroundKillAgent {
  /** The launch row the agent's run state is served on. */
  launchItemId: string;
  /** The agent's task line. */
  description: string;
  /** 'parked': the agent reported and waits on its own background commands. */
  runState: BackgroundAgentRunState;
  /** Opens the agent's pane (agentScopeRootId). */
  transcriptRootId: string;
}

export interface BackgroundKillRefusal {
  /** The backend's sentence, safe to show as is. */
  message: string;
  /** Empty when the backend's list was missing or unreadable. */
  agents: BackgroundKillAgent[];
}

export interface PendingBackgroundKill extends BackgroundKillRefusal {
  threadId: string;
}

const MAX_AGENTS = 64;
const MAX_ID = 256;
const MAX_DESCRIPTION = 512;

function itemId(value: unknown): string | null {
  return typeof value === 'string' && value.length > 0 && value.length <= MAX_ID ? value : null;
}

function decodeAgent(value: unknown): BackgroundKillAgent | null {
  if (typeof value !== 'object' || value === null) return null;
  const raw = value as Record<string, unknown>;
  const launchItemId = itemId(raw.launchItemId);
  const transcriptRootId = itemId(raw.transcriptRootId);
  const runState = raw.runState;
  if (!launchItemId || !transcriptRootId || typeof raw.description !== 'string') return null;
  if (runState !== 'running' && runState !== 'parked') return null;
  return { launchItemId, description: raw.description.slice(0, MAX_DESCRIPTION), runState, transcriptRootId };
}

/**
 * The refusal an interrupt RPC rejected with, or null for any other error.
 * The agent list comes from the backend unvalidated: malformed entries are
 * dropped, never shown.
 */
export function backgroundKillRefusal(err: unknown): BackgroundKillRefusal | null {
  if (!(err instanceof TransportError) || err.code !== BACKGROUND_AGENTS_RUNNING) return null;
  const agents: BackgroundKillAgent[] = [];
  if (Array.isArray(err.backgroundAgents)) {
    for (const value of err.backgroundAgents.slice(0, MAX_AGENTS)) {
      const agent = decodeAgent(value);
      if (agent) agents.push(agent);
    }
  }
  return { message: err.message, agents };
}

const pending = createKeyedSignalRegistry<PendingBackgroundKill | null>(null);
const pendingThreads = new Set<string>();

export function offerBackgroundKillConfirmation(threadId: string, refusal: BackgroundKillRefusal): void {
  if (!threadId) return;
  pendingThreads.add(threadId);
  pending.set(threadId, { threadId, message: refusal.message, agents: refusal.agents });
}

export function pendingBackgroundKill(threadId: string | null | undefined): PendingBackgroundKill | null {
  return threadId ? pending.get(threadId) : null;
}

export function dismissBackgroundKill(threadId: string): void {
  if (!pendingThreads.delete(threadId)) return;
  pending.drop(threadId);
}

export function resetBackgroundKillConfirmationsForTest(): void {
  pendingThreads.clear();
  pending.reset();
}

onThreadHistoryInvalidated((owns) => {
  for (const threadId of [...pendingThreads]) {
    if (owns(threadId)) dismissBackgroundKill(threadId);
  }
});

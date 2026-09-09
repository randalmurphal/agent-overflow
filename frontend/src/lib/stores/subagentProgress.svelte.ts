// Live per-subagent progress counters, from `provider:subagent_progress`.
//
// Live session state, never history: Claude emits a `task_progress` tick
// after every tool round of a running agent and Codex a token-usage
// frame per child turn. The agent card reads the latest tick while the
// agent runs (tool count, tokens, elapsed, activity line); once the
// execution settles, the final numbers live on its completion meta
// (`meta.subagentProgress`, persisted by triage at the terminal) and the
// card reads those instead — see `utils/subagentProgress.ts`.
//
// One reactive box per (thread, launch) via keyedSignalRegistry: every
// agent card on screen reads its own key, and a shared map would wake all
// of them on any agent's tick. Entries die with the session (thread
// teardown drops them); a refresh starts empty and the next tick refills,
// which is the same answer the backend gives — nothing is persisted on
// either side until the terminal.

import type { Item } from '../types/models';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import type { SubagentProgress, SubagentProgressEvent } from '../types/events';

const EMPTY: SubagentProgress | undefined = undefined;

const progressByKey = createKeyedSignalRegistry<SubagentProgress | undefined>(EMPTY);
const codexAgents = createKeyedSignalRegistry<Item | undefined>(undefined);
let codexRevision = 0;
const codexRevisions = new Map<string, number>();

export function codexAgentRevision(threadId: string): number { return codexRevisions.get(threadId) ?? 0; }

export function liveCodexAgent(threadId: string, itemId: string): Item | undefined {
  return codexAgents.get(progressKey(threadId, itemId));
}

export function hydrateCodexAgents(threadId: string, items: Item[], revision: number): void {
  if (revision !== codexAgentRevision(threadId)) return;
  for (const key of keysByThread.get(threadId) ?? []) codexAgents.drop(key);
  for (const item of items) {
    if (item.threadId !== threadId) continue;
    const key = progressKey(threadId, item.id);
    codexAgents.set(key, item);
    let keys = keysByThread.get(threadId);
    if (!keys) keysByThread.set(threadId, keys = new Set());
    keys.add(key);
  }
  codexRevisions.set(threadId, ++codexRevision);
}

const keysByThread = new Map<string, Set<string>>();

function progressKey(threadId: string, itemId: string): string {
  return `${threadId}:${itemId}`;
}

/** Tracked read: the latest live tick for a launch, or undefined. */
export function liveSubagentProgress(
  threadId: string | null | undefined,
  itemId: string | null | undefined,
): SubagentProgress | undefined {
  if (!threadId || !itemId) return undefined;
  return progressByKey.get(progressKey(threadId, itemId));
}

/** Apply a `provider:subagent_progress` frame. Triage already merged the
 * tick over the previous one, so the frame is the whole answer. */
export function applySubagentProgress(evt: SubagentProgressEvent | undefined): void {
  if (!evt || !evt.threadId || !evt.itemId || !evt.progress) return;
  const key = progressKey(evt.threadId, evt.itemId);
  if (evt.codexAgent && evt.codexAgent.threadId === evt.threadId && evt.codexAgent.id === evt.itemId) {
    codexAgents.set(key, evt.codexAgent);
    codexRevisions.set(evt.threadId, ++codexRevision);
  }
  progressByKey.set(key, { ...evt.progress, updatedAt: evt.updatedAt ?? 0 });
  let keys = keysByThread.get(evt.threadId);
  if (!keys) {
    keys = new Set();
    keysByThread.set(evt.threadId, keys);
  }
  keys.add(key);
}

/** Drop one launch's live tick — its row settled and the persisted final
 * numbers take over. */
export function dropSubagentProgress(threadId: string, itemId: string): void {
  if (!threadId || !itemId) return;
  const key = progressKey(threadId, itemId);
  progressByKey.drop(key);
  if (!codexAgents.get(key)) keysByThread.get(threadId)?.delete(key);
}

/** Drop a thread's ticks — session teardown, thread delete/archive. */
export function clearSubagentProgressForThread(threadId: string): void {
  if (!threadId) return;
  const keys = keysByThread.get(threadId);
  codexRevisions.set(threadId, ++codexRevision);
  if (!keys) return;
  for (const key of keys) { progressByKey.drop(key); codexAgents.drop(key); }
  keysByThread.delete(threadId);
}

/** Test-only fixture isolation, matching the sibling stores. */
export function resetForTest(): void {
  progressByKey.reset();
  codexAgents.reset();
  codexRevisions.clear();
  codexRevision++;
  keysByThread.clear();
}

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
// teardown drops them). The backend sends a thread's ticks only while this
// client watches it, so a pane that opens hydrates the thread's live
// entries from GetThreadLiveState (hydrateSubagentProgress); nothing is
// persisted on either side until the terminal.

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
// Revisions of a thread's live ticks: the thread's latest change, each
// launch's latest tick or drop, and the thread's latest clear. A snapshot
// read at an older revision never undoes a change that landed after the
// read (hydrateSubagentProgress). A thread's launch revisions die with its
// clear.
let progressRevision = 0;
const threadRevisions = new Map<string, number>();
const launchRevisions = new Map<string, Map<string, number>>();
const clearRevisions = new Map<string, number>();

export function subagentProgressRevision(threadId: string): number { return threadRevisions.get(threadId) ?? 0; }

function noteProgressChange(threadId: string, itemId: string): void {
  const revision = ++progressRevision;
  threadRevisions.set(threadId, revision);
  let launches = launchRevisions.get(threadId);
  if (!launches) launchRevisions.set(threadId, launches = new Map());
  launches.set(itemId, revision);
}

/**
 * Install a thread's live ticks from a GetThreadLiveState snapshot read at
 * `revision`. The snapshot answers for every launch that has not changed
 * since the read: it sets the ones it lists and drops the ones it omits,
 * which have settled. A launch that ticked or settled after the read keeps
 * this client's state, and a snapshot read before the thread was cleared
 * is refused.
 */
export function hydrateSubagentProgress(threadId: string, entries: readonly SubagentProgressEvent[], revision: number): void {
  if (!threadId || revision < (clearRevisions.get(threadId) ?? 0)) return;
  const launches = launchRevisions.get(threadId);
  const changedSinceRead = (itemId: string) => (launches?.get(itemId) ?? 0) > revision;
  const named = new Set<string>();
  let keys = keysByThread.get(threadId);
  for (const entry of entries) {
    if (entry.threadId !== threadId || !entry.itemId || !entry.progress) continue;
    const key = progressKey(threadId, entry.itemId);
    named.add(key);
    if (changedSinceRead(entry.itemId)) continue;
    progressByKey.set(key, { ...entry.progress, updatedAt: entry.updatedAt ?? 0 });
    if (!keys) keysByThread.set(threadId, keys = new Set());
    keys.add(key);
  }
  for (const key of keys ?? []) {
    const itemId = key.slice(threadId.length + 1);
    if (named.has(key) || changedSinceRead(itemId) || !progressByKey.get(key)) continue;
    progressByKey.drop(key);
    if (!codexAgents.get(key)) keys!.delete(key);
  }
}

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
  noteProgressChange(evt.threadId, evt.itemId);
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
  noteProgressChange(threadId, itemId);
  if (!codexAgents.get(key)) keysByThread.get(threadId)?.delete(key);
}

/** Drop a thread's ticks — session teardown, thread delete/archive. */
export function clearSubagentProgressForThread(threadId: string): void {
  if (!threadId) return;
  const keys = keysByThread.get(threadId);
  codexRevisions.set(threadId, ++codexRevision);
  const revision = ++progressRevision;
  threadRevisions.set(threadId, revision);
  clearRevisions.set(threadId, revision);
  launchRevisions.delete(threadId);
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
  threadRevisions.clear();
  launchRevisions.clear();
  clearRevisions.clear();
  progressRevision++;
  keysByThread.clear();
}

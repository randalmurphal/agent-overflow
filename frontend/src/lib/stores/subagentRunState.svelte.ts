// Live run states of a thread's Claude background agents, as the tray's
// `ListLiveBackgroundTasks` reads serve them (claude-wire.md §E6b).
//
// Live session state, never history: a park or a wake moves no row, so
// the only source is a list read, and the tray controller
// (components/composer/activityRailBackground.svelte.ts) writes every
// read's answer here wholesale. Readers are the tray row and any surface
// that must show whether an agent is running, parked, done or ended.
//
// One reactive box per (thread, launch) via keyedSignalRegistry, as
// subagentProgress does: a row re-evaluates only when ITS launch's state
// changes, and a read that serves the same state again rewrites nothing.
// Entries die with the thread (teardown drops them); the next read after
// a pane returns refills them.

import { untrack } from 'svelte';
import type { SubagentRunState } from '../utils/subagentRunState';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

const statesByKey = createKeyedSignalRegistry<SubagentRunState | null>(null);
const keysByThread = new Map<string, Set<string>>();

function stateKey(threadId: string, launchId: string): string {
  return `${threadId}:${launchId}`;
}

function sameRunState(a: SubagentRunState, b: SubagentRunState): boolean {
  return a.state === b.state
    && a.waitingOn === b.waitingOn
    && a.report?.id === b.report?.id
    && a.report?.preview === b.report?.preview;
}

/** Tracked read: the served run state of a launch, or null. */
export function liveSubagentRunState(
  threadId: string | null | undefined,
  launchId: string | null | undefined,
): SubagentRunState | null {
  if (!threadId || !launchId) return null;
  return statesByKey.get(stateKey(threadId, launchId));
}

/**
 * Untracked: whether any launch of the thread is served as running or
 * parked, the states a Claude interrupt kills. Read by a Stop before it
 * clears the working presentation, so a Stop the backend will refuse
 * until the person confirms does not flash the thread idle first. The
 * backend's refusal stays the authority; this only decides the optimistic
 * presentation.
 */
export function hasLiveSubagentRunStates(threadId: string | null | undefined): boolean {
  if (!threadId) return false;
  const keys = keysByThread.get(threadId);
  if (!keys) return false;
  return untrack(() => {
    for (const key of keys) {
      const state = statesByKey.get(key);
      if (state?.state === 'running' || state?.state === 'parked') return true;
    }
    return false;
  });
}

/**
 * Replace a thread's run states with one list read's answer. A launch the
 * read no longer lists is dropped; a launch whose state is unchanged is
 * left alone so its readers do not wake.
 */
export function replaceSubagentRunStates(
  threadId: string,
  states: ReadonlyMap<string, SubagentRunState>,
): void {
  if (!threadId) return;
  const keys = keysByThread.get(threadId) ?? new Set<string>();
  const next = new Set<string>();
  for (const [launchId, state] of states) {
    const key = stateKey(threadId, launchId);
    next.add(key);
    const current = statesByKey.get(key);
    if (current === null || !sameRunState(current, state)) statesByKey.set(key, state);
  }
  for (const key of keys) {
    if (!next.has(key)) statesByKey.drop(key);
  }
  if (next.size === 0) keysByThread.delete(threadId);
  else keysByThread.set(threadId, next);
}

/** Drop a thread's run states: session teardown, thread delete/archive. */
export function clearSubagentRunStatesForThread(threadId: string): void {
  if (!threadId) return;
  const keys = keysByThread.get(threadId);
  if (!keys) return;
  for (const key of keys) statesByKey.drop(key);
  keysByThread.delete(threadId);
}

/** Test-only fixture isolation, matching the sibling stores. */
export function resetForTest(): void {
  statesByKey.reset();
  keysByThread.clear();
}

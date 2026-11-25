// Bounded LRU pool of detached live xterm instances (WebGL2 renderer only)
// so toggling the terminal drawer or switching terminal tabs reattaches the
// existing WebGL2 canvas + glyph atlas in <2ms instead of tearing down and
// recompiling WebGL2 shaders (~150ms GPU stall per toggle).
//
// Instances are evicted immediately when their tab exits, their pane leaves
// the layout, their pending output overflows while parked, the LRU cap is
// reached, or they sit unused past the idle window.

import { onPaneLayoutItemsRemoved } from '../../stores/paneLayout.svelte';
import type { BackendKey } from '../../transport/backendKey';
import type { BuiltTerminal } from './terminalXterm';

export interface WarmTerminalEntry {
  terminalID: string;
  handle: object;
  paneId: string;
  backend: BackendKey;
  built: BuiltTerminal;
}

interface StoredWarmTerminal extends WarmTerminalEntry {
  timer: ReturnType<typeof setTimeout> | null;
}

const MAX_WARM_TERMINALS = 4;
const IDLE_EVICT_MS = 120_000;

const pool = new Map<string, StoredWarmTerminal>();
// Panes removed during the current layout-teardown flush. `replaceLayoutItems`
// notifies `onPaneLayoutItemsRemoved` synchronously before Svelte's microtask
// unmounts `<TerminalBody>`, so `onDestroy` can call `parkWarmTerminal` after
// `evictWarmTerminalsForPane` already ran. Rejecting parks for panes in this
// set closes that teardown-order leak.
const removingPanes = new Set<string>();
let removingEpoch = 0;

onPaneLayoutItemsRemoved((paneIds) => {
  for (const paneId of paneIds) {
    removingPanes.add(paneId);
    evictWarmTerminalsForPane(paneId);
  }
  const epoch = ++removingEpoch;
  queueMicrotask(() => {
    queueMicrotask(() => {
      if (removingEpoch === epoch) removingPanes.clear();
    });
  });
});

function disposeEntry(entry: StoredWarmTerminal): void {
  if (entry.timer !== null) {
    clearTimeout(entry.timer);
    entry.timer = null;
  }
  entry.built.dispose();
}

export function parkWarmTerminal(entry: WarmTerminalEntry): void {
  if (removingPanes.has(entry.paneId)) {
    entry.built.dispose();
    return;
  }
  const existing = pool.get(entry.terminalID);
  if (existing) {
    pool.delete(entry.terminalID);
    disposeEntry(existing);
  }
  while (pool.size >= MAX_WARM_TERMINALS) {
    const oldestKey = pool.keys().next().value;
    if (oldestKey === undefined) break;
    const oldest = pool.get(oldestKey);
    pool.delete(oldestKey);
    if (oldest) disposeEntry(oldest);
  }
  const stored: StoredWarmTerminal = {
    ...entry,
    timer: setTimeout(() => {
      if (pool.get(entry.terminalID) !== stored) return;
      pool.delete(entry.terminalID);
      stored.timer = null;
      stored.built.dispose();
    }, IDLE_EVICT_MS),
  };
  pool.set(entry.terminalID, stored);
}

export function takeWarmTerminal(
  handle: object,
  terminalID: string,
): BuiltTerminal | null {
  const entry = pool.get(terminalID);
  if (!entry) return null;
  if (entry.handle !== handle || !entry.built.hasWebgl) {
    pool.delete(terminalID);
    disposeEntry(entry);
    return null;
  }
  pool.delete(terminalID);
  if (entry.timer !== null) {
    clearTimeout(entry.timer);
    entry.timer = null;
  }
  return entry.built;
}

export function evictWarmTerminal(terminalID: string): void {
  const entry = pool.get(terminalID);
  if (!entry) return;
  pool.delete(terminalID);
  disposeEntry(entry);
}

export function evictWarmTerminalsForHandle(handle: object): void {
  for (const [id, entry] of pool) {
    if (entry.handle !== handle) continue;
    pool.delete(id);
    disposeEntry(entry);
  }
}

export function evictWarmTerminalsForPane(paneId: string): void {
  for (const [id, entry] of pool) {
    if (entry.paneId !== paneId) continue;
    pool.delete(id);
    disposeEntry(entry);
  }
}

export function evictWarmTerminalsForBackend(backend: BackendKey): void {
  for (const [id, entry] of pool) {
    if (entry.backend !== backend) continue;
    pool.delete(id);
    disposeEntry(entry);
  }
}

export function clearWarmTerminals(): void {
  for (const entry of pool.values()) disposeEntry(entry);
  pool.clear();
  removingEpoch++;
  removingPanes.clear();
}

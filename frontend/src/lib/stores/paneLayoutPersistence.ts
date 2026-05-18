import type { Thread } from '../types/models';
import { debounce } from '../utils/debounce';
import { ListThreads } from './bindings';
import {
  DEFAULT_PANE_RATIO,
  getPaneLayoutItems,
  setPaneLayoutPersistenceHandlers,
  setPaneLayoutItems,
  type PaneLayoutItem,
} from './paneLayout.svelte';
import {
  getAllPanes,
  getFocusedPaneId,
  hydrateRestoredPaneRegistry,
  resetPaneRegistry,
  setPanePersistenceHandler,
} from './panes.svelte';

export const PANE_LAYOUT_STORAGE_KEY = 'agentOverflowPaneLayout';
const PANE_LAYOUT_STORAGE_VERSION = 1;
const PANE_RESIZE_PERSIST_DELAY_MS = 200;
const MAX_RESTORED_PANES = 24;
const SAFE_PANE_ID_PATTERN = /^[A-Za-z0-9_-]+$/;

interface PersistedPane {
  paneId: string;
  threadId: string;
  ratio: number;
}

interface PersistedPaneLayout {
  version: 1;
  panes: PersistedPane[];
  focusedPaneId: string | null;
}

function storageAvailable(): boolean {
  return typeof localStorage !== 'undefined';
}

function normalizePersistedRatio(ratio: unknown): number {
  return typeof ratio === 'number' && Number.isFinite(ratio) && ratio > 0
    ? ratio
    : DEFAULT_PANE_RATIO;
}

function isSafePersistedPaneId(paneId: string): boolean {
  return paneId.length > 0 && SAFE_PANE_ID_PATTERN.test(paneId);
}

async function emptyLayout(): Promise<void> {
  setPaneLayoutItems([]);
  resetPaneRegistry(null);
}

function readLayoutStorage(): string | null {
  if (!storageAvailable()) return null;
  try {
    return localStorage.getItem(PANE_LAYOUT_STORAGE_KEY);
  } catch (err) {
    console.warn('Failed to read pane layout persistence:', err);
    return null;
  }
}

function writeLayoutStorage(value: string): void {
  if (!storageAvailable()) return;
  try {
    localStorage.setItem(PANE_LAYOUT_STORAGE_KEY, value);
  } catch (err) {
    console.warn('Failed to write pane layout persistence:', err);
  }
}

function removeLayoutStorage(): void {
  if (!storageAvailable()) return;
  try {
    localStorage.removeItem(PANE_LAYOUT_STORAGE_KEY);
  } catch (err) {
    console.warn('Failed to clear pane layout persistence:', err);
  }
}

function parsePersistedLayout(raw: string | null): PersistedPaneLayout | null {
  if (!raw) return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!parsed || typeof parsed !== 'object') return null;
  const record = parsed as Record<string, unknown>;
  if (record.version !== PANE_LAYOUT_STORAGE_VERSION) return null;
  if (!Array.isArray(record.panes)) return null;
  const focusedPaneId = typeof record.focusedPaneId === 'string' && isSafePersistedPaneId(record.focusedPaneId)
    ? record.focusedPaneId
    : null;
  const panes: PersistedPane[] = [];
  const seenPaneIds = new Set<string>();
  const seenThreadIds = new Set<string>();
  for (const item of record.panes) {
    if (!item || typeof item !== 'object') continue;
    const pane = item as Record<string, unknown>;
    if (typeof pane.paneId !== 'string' || !isSafePersistedPaneId(pane.paneId)) continue;
    if (typeof pane.threadId !== 'string' || pane.threadId.length === 0) continue;
    if (seenPaneIds.has(pane.paneId) || seenThreadIds.has(pane.threadId)) continue;
    seenPaneIds.add(pane.paneId);
    seenThreadIds.add(pane.threadId);
    panes.push({
      paneId: pane.paneId,
      threadId: pane.threadId,
      ratio: normalizePersistedRatio(pane.ratio),
    });
    if (panes.length >= MAX_RESTORED_PANES) break;
  }
  return { version: PANE_LAYOUT_STORAGE_VERSION, panes, focusedPaneId };
}

function buildSnapshot(): PersistedPaneLayout | null {
  const panesById = getAllPanes();
  const panes: PersistedPane[] = [];
  for (const item of getPaneLayoutItems()) {
    const threadId = panesById.get(item.paneId)?.threadId;
    if (!threadId) continue;
    panes.push({
      paneId: item.paneId,
      threadId,
      ratio: normalizePersistedRatio(item.ratio),
    });
  }
  if (panes.length === 0) return null;
  const focusedPaneId = getFocusedPaneId();
  return {
    version: PANE_LAYOUT_STORAGE_VERSION,
    panes,
    focusedPaneId: focusedPaneId && panes.some((pane) => pane.paneId === focusedPaneId)
      ? focusedPaneId
      : panes[0]?.paneId ?? null,
  };
}

async function loadThreadsForValidation(availableThreads?: Thread[]): Promise<Thread[]> {
  if (availableThreads) return availableThreads;
  return await ListThreads() as Thread[];
}

export async function loadFromStorage(availableThreads?: Thread[]): Promise<void> {
  if (!storageAvailable()) {
    await emptyLayout();
    return;
  }

  const persisted = parsePersistedLayout(readLayoutStorage());
  if (!persisted) {
    await emptyLayout();
    return;
  }

  const threads = await loadThreadsForValidation(availableThreads);
  const threadById = new Map(threads.map((thread) => [thread.id, thread]));
  const layoutItems: PaneLayoutItem[] = [];
  const registryEntries: Array<{ paneId: string; thread: Thread }> = [];
  for (const pane of persisted.panes) {
    const thread = threadById.get(pane.threadId);
    if (!thread) continue;
    layoutItems.push({
      id: pane.paneId,
      paneId: pane.paneId,
      kind: 'thread',
      ratio: pane.ratio,
    });
    registryEntries.push({ paneId: pane.paneId, thread });
  }

  const restoredFocusedPaneId = persisted.focusedPaneId &&
    registryEntries.some((entry) => entry.paneId === persisted.focusedPaneId)
    ? persisted.focusedPaneId
    : registryEntries[0]?.paneId ?? null;
  setPaneLayoutItems(layoutItems);
  await hydrateRestoredPaneRegistry(registryEntries, restoredFocusedPaneId);
}

export function persistToStorage(): void {
  if (!storageAvailable()) return;
  const snapshot = buildSnapshot();
  if (!snapshot) {
    // Empty layout is encoded as a missing key. That keeps "no saved layout"
    // and "user closed every pane" on the same empty-state path at launch.
    removeLayoutStorage();
    return;
  }
  writeLayoutStorage(JSON.stringify(snapshot));
}

export const persistToStorageDebounced = debounce(persistToStorage, PANE_RESIZE_PERSIST_DELAY_MS);

export function installPaneLayoutPersistence(): void {
  setPaneLayoutPersistenceHandlers({
    immediate: persistToStorage,
    debounced: persistToStorageDebounced,
  });
  setPanePersistenceHandler(persistToStorage);
}

export function resetPaneLayoutPersistenceForTest(): void {
  persistToStorageDebounced.cancel();
  setPaneLayoutPersistenceHandlers(null);
  setPanePersistenceHandler(null);
}

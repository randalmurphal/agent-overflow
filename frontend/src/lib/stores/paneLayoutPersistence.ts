import type { Thread } from '../types/models';
import type {
  PaneLayoutPersistedPane,
  PaneLayoutPersistedSettings,
} from '../types/settings';
import { debounce } from '../utils/debounce';
import {
  appStorageAdoptLegacyKey,
  appStorageGet,
  appStorageSet,
  flushAppStorage,
} from './appStorage';
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

// Persisted per client via appStorage (ui_state table): pane layout is
// view state, and two clients of the same backend keep independent
// layouts. Pre-appStorage copies migrate in transparently: the Go
// startup migration moves the old settings.json paneLayout into the
// embedded client's bucket, and the ancient localStorage key below is
// adopted at read time.
const PANE_LAYOUT_KEY = 'paneLayout';
const LEGACY_PANE_LAYOUT_STORAGE_KEY = 'agentOverflowPaneLayout';
const PANE_LAYOUT_SETTINGS_VERSION = 1;
const PANE_RESIZE_PERSIST_DELAY_MS = 200;
const MAX_RESTORED_PANES = 24;
const MAX_PANE_ID_LENGTH = 64;
const MAX_THREAD_ID_LENGTH = 256;
const MAX_PANE_RATIO = 100;
const SAFE_PANE_ID_PATTERN = /^[A-Za-z0-9_-]+$/;

type PersistedPaneLayout = PaneLayoutPersistedSettings;
type PersistedPane = PaneLayoutPersistedPane;

let lastPersistedPaneLayoutKey: string | null = null;

function normalizePersistedRatio(ratio: unknown): number {
  if (typeof ratio !== 'number' || !Number.isFinite(ratio) || ratio <= 0) {
    return DEFAULT_PANE_RATIO;
  }
  return Math.min(ratio, MAX_PANE_RATIO);
}

function isSafePersistedPaneId(paneId: string): boolean {
  return paneId.length > 0 &&
    paneId.length <= MAX_PANE_ID_LENGTH &&
    SAFE_PANE_ID_PATTERN.test(paneId);
}

function isSafePersistedThreadId(threadId: string): boolean {
  return threadId.length > 0 && threadId.length <= MAX_THREAD_ID_LENGTH;
}

async function emptyLayout(): Promise<void> {
  setPaneLayoutItems([]);
  resetPaneRegistry(null);
}

function parsePersistedLayout(raw: unknown): PersistedPaneLayout | null {
  if (!raw || typeof raw !== 'object') return null;
  const record = raw as Record<string, unknown>;
  if (record.version !== PANE_LAYOUT_SETTINGS_VERSION) return null;
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
    if (typeof pane.threadId !== 'string' || !isSafePersistedThreadId(pane.threadId)) continue;
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
  return { version: PANE_LAYOUT_SETTINGS_VERSION, panes, focusedPaneId };
}

function paneLayoutKey(snapshot: PersistedPaneLayout): string {
  return JSON.stringify(snapshot);
}

function buildSnapshot(): PersistedPaneLayout {
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
  const focusedPaneId = getFocusedPaneId();
  return {
    version: PANE_LAYOUT_SETTINGS_VERSION,
    panes,
    focusedPaneId: focusedPaneId && panes.some((pane) => pane.paneId === focusedPaneId)
      ? focusedPaneId
      : panes[0]?.paneId ?? null,
  };
}

function parsePersistedLayoutJSON(raw: string): PersistedPaneLayout | null {
  try {
    return parsePersistedLayout(JSON.parse(raw));
  } catch {
    return null;
  }
}

function readPersistedLayout(): PersistedPaneLayout | null {
  const raw =
    appStorageGet(PANE_LAYOUT_KEY) ??
    appStorageAdoptLegacyKey(PANE_LAYOUT_KEY, LEGACY_PANE_LAYOUT_STORAGE_KEY, (legacy) =>
      parsePersistedLayoutJSON(legacy) === null ? null : legacy,
    );
  if (raw === null) return null;
  const layout = parsePersistedLayoutJSON(raw);
  if (layout) {
    lastPersistedPaneLayoutKey = paneLayoutKey(layout);
  }
  return layout;
}

async function loadThreadsForValidation(availableThreads?: Thread[]): Promise<Thread[]> {
  if (availableThreads) return availableThreads;
  return await ListThreads() as Thread[];
}

/**
 * Restore the persisted pane layout into the layout + pane registry
 * stores. Callers must run this AFTER hydrateAppStorage() has resolved
 * — before that, only the same-session cache is visible and a fresh
 * launch would restore an empty layout.
 */
export async function loadPersistedPaneLayout(availableThreads?: Thread[]): Promise<void> {
  const persisted = readPersistedLayout();
  if (!persisted) {
    await emptyLayout();
    return;
  }

  const threads = await loadThreadsForValidation(availableThreads);
  const neededThreadIds = new Set(persisted.panes.map((pane) => pane.threadId));
  const threadById = new Map<string, Thread>();
  for (const thread of threads) {
    const threadId = thread.id;
    if (neededThreadIds.has(threadId)) threadById.set(threadId, thread);
  }
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

export function persistPaneLayout(): void {
  const snapshot = buildSnapshot();
  const key = paneLayoutKey(snapshot);
  if (key === lastPersistedPaneLayoutKey) return;
  lastPersistedPaneLayoutKey = key;
  appStorageSet(PANE_LAYOUT_KEY, key);
}

export const persistPaneLayoutDebounced = debounce(persistPaneLayout, PANE_RESIZE_PERSIST_DELAY_MS);

async function flushPendingPaneLayoutPersistence(): Promise<void> {
  persistPaneLayoutDebounced.flush();
  await flushAppStorage();
}

export function installPaneLayoutPersistence(): void {
  setPaneLayoutPersistenceHandlers({
    immediate: persistPaneLayout,
    debounced: persistPaneLayoutDebounced,
    flush: flushPendingPaneLayoutPersistence,
  });
  setPanePersistenceHandler(persistPaneLayout);
}

export async function waitForPaneLayoutPersistenceForTest(): Promise<void> {
  await flushAppStorage();
}

export function resetPaneLayoutPersistenceForTest(): void {
  persistPaneLayoutDebounced.cancel();
  lastPersistedPaneLayoutKey = null;
  setPaneLayoutPersistenceHandlers(null);
  setPanePersistenceHandler(null);
}

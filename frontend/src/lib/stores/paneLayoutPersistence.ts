import type { Thread } from '../types/models';
import type {
  PaneLayoutPersistedPane,
  PaneLayoutPersistedSettings,
  Settings,
} from '../types/settings';
import { debounce } from '../utils/debounce';
import { GetSettings, ListThreads, UpdateSettings } from './bindings';
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

let paneLayoutWriteInFlight = false;
let pendingPaneLayoutSnapshot: PersistedPaneLayout | null = null;
let lastPersistedPaneLayoutKey: string | null = null;
let paneLayoutWriteIdlePromise: Promise<void> = Promise.resolve();
let resolvePaneLayoutWriteIdlePromise: (() => void) | null = null;

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

function isEmptyPaneLayout(snapshot: PersistedPaneLayout): boolean {
  return snapshot.panes.length === 0 && !snapshot.focusedPaneId;
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

function readLegacyPersistedLayout(): PersistedPaneLayout | null {
  if (typeof localStorage === 'undefined') return null;
  let raw: string | null;
  try {
    raw = localStorage.getItem(LEGACY_PANE_LAYOUT_STORAGE_KEY);
  } catch (err) {
    console.warn('Failed to read legacy pane layout persistence:', err);
    return null;
  }
  if (!raw) return null;
  try {
    return parsePersistedLayout(JSON.parse(raw));
  } catch {
    return null;
  }
}

function removeLegacyPersistedLayout(): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.removeItem(LEGACY_PANE_LAYOUT_STORAGE_KEY);
  } catch (err) {
    console.warn('Failed to clear legacy pane layout persistence:', err);
  }
}

async function readPersistedLayout(): Promise<PersistedPaneLayout | null> {
  const settings = await GetSettings() as Settings | null;
  const settingsLayout = parsePersistedLayout(settings?.paneLayout ?? null);
  if (settingsLayout && !isEmptyPaneLayout(settingsLayout)) {
    lastPersistedPaneLayoutKey = paneLayoutKey(settingsLayout);
    return settingsLayout;
  }
  const legacyLayout = readLegacyPersistedLayout();
  if (legacyLayout && !isEmptyPaneLayout(legacyLayout)) {
    queuePaneLayoutWrite(legacyLayout);
    return legacyLayout;
  }
  if (settingsLayout) {
    lastPersistedPaneLayoutKey = paneLayoutKey(settingsLayout);
  }
  return settingsLayout;
}

function resolvePaneLayoutWriteIdle(): void {
  const resolve = resolvePaneLayoutWriteIdlePromise;
  resolvePaneLayoutWriteIdlePromise = null;
  resolve?.();
}

function ensurePaneLayoutWriteIdlePromise(): void {
  if (resolvePaneLayoutWriteIdlePromise) return;
  paneLayoutWriteIdlePromise = new Promise((resolve) => {
    resolvePaneLayoutWriteIdlePromise = resolve;
  });
}

function startPaneLayoutWriteLoop(): void {
  if (paneLayoutWriteInFlight) return;
  paneLayoutWriteInFlight = true;
  ensurePaneLayoutWriteIdlePromise();
  void (async () => {
    try {
      while (pendingPaneLayoutSnapshot) {
        const snapshot = pendingPaneLayoutSnapshot;
        pendingPaneLayoutSnapshot = null;
        const key = paneLayoutKey(snapshot);
        if (key === lastPersistedPaneLayoutKey) continue;
        try {
          await UpdateSettings({ paneLayout: snapshot } satisfies Partial<Settings>);
          lastPersistedPaneLayoutKey = key;
          removeLegacyPersistedLayout();
        } catch (err) {
          console.warn('Failed to write pane layout persistence:', err);
        }
      }
    } finally {
      paneLayoutWriteInFlight = false;
      if (pendingPaneLayoutSnapshot) {
        startPaneLayoutWriteLoop();
      } else {
        resolvePaneLayoutWriteIdle();
      }
    }
  })();
}

function queuePaneLayoutWrite(snapshot: PersistedPaneLayout): void {
  if (paneLayoutKey(snapshot) === lastPersistedPaneLayoutKey && !pendingPaneLayoutSnapshot) {
    return;
  }
  pendingPaneLayoutSnapshot = snapshot;
  startPaneLayoutWriteLoop();
}

async function loadThreadsForValidation(availableThreads?: Thread[]): Promise<Thread[]> {
  if (availableThreads) return availableThreads;
  return await ListThreads() as Thread[];
}

export async function loadFromSettings(availableThreads?: Thread[]): Promise<void> {
  const persisted = await readPersistedLayout();
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

export function persistToSettings(): void {
  queuePaneLayoutWrite(buildSnapshot());
}

export const persistToSettingsDebounced = debounce(persistToSettings, PANE_RESIZE_PERSIST_DELAY_MS);

async function flushPendingPaneLayoutPersistence(): Promise<void> {
  persistToSettingsDebounced.flush();
  await paneLayoutWriteIdlePromise;
}

export function installPaneLayoutPersistence(): void {
  setPaneLayoutPersistenceHandlers({
    immediate: persistToSettings,
    debounced: persistToSettingsDebounced,
    flush: flushPendingPaneLayoutPersistence,
  });
  setPanePersistenceHandler(persistToSettings);
}

export async function waitForPaneLayoutPersistenceForTest(): Promise<void> {
  await paneLayoutWriteIdlePromise;
}

export function resetPaneLayoutPersistenceForTest(): void {
  persistToSettingsDebounced.cancel();
  paneLayoutWriteInFlight = false;
  pendingPaneLayoutSnapshot = null;
  lastPersistedPaneLayoutKey = null;
  paneLayoutWriteIdlePromise = Promise.resolve();
  resolvePaneLayoutWriteIdlePromise = null;
  setPaneLayoutPersistenceHandlers(null);
  setPanePersistenceHandler(null);
}

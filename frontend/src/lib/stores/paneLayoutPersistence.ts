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
  getPaneLayoutItems,
  setPaneLayoutPersistenceHandlers,
  setPaneLayoutItems,
  type PaneLayoutItem,
} from './paneLayout.svelte';
import { getMinPaneWidth } from './paneDensity.svelte';
import { FALLBACK_PANE_WIDTH_PX, normalizePaneWidthPx } from '../utils/paneWidths';
import { restoreCompanion, type CompanionPanelKind } from './companionPanes.svelte';
import {
  getPersistedWorkflowsPaneState,
  parsePersistedWorkflowsPaneState,
  resetWorkflowsPane,
  restoreWorkflowsPaneState,
  setWorkflowsPanePersistenceHandler,
} from './workflowsPane.svelte';
import {
  focusPane,
  getAllPanes,
  getFocusedPaneId,
  getFocusedThreadPaneId,
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
const PANE_LAYOUT_SETTINGS_VERSION = 3;
const PANE_RESIZE_PERSIST_DELAY_MS = 200;
const MAX_RESTORED_PANES = 24;
const MAX_PANE_ID_LENGTH = 64;
const MAX_THREAD_ID_LENGTH = 256;
// Cap for pre-v3 persisted flex ratios during migration.
const MAX_LEGACY_PANE_RATIO = 100;
const SAFE_PANE_ID_PATTERN = /^[A-Za-z0-9_-]+$/;

type PersistedPaneLayout = PaneLayoutPersistedSettings;
type PersistedPane = PaneLayoutPersistedPane;

let lastPersistedPaneLayoutKey: string | null = null;

function normalizePersistedWidthPx(widthPx: unknown): number {
  if (typeof widthPx !== 'number') return FALLBACK_PANE_WIDTH_PX;
  return normalizePaneWidthPx(widthPx);
}

// Versions 1-2 persisted flex-grow ratios (typically ~1). Migrate to
// absolute widths by scaling the current density minimum, preserving
// any proportions a zero-sum resize had produced.
function migrateLegacyRatioToWidthPx(ratio: unknown): number {
  if (typeof ratio !== 'number' || !Number.isFinite(ratio) || ratio <= 0) {
    return normalizePaneWidthPx(getMinPaneWidth());
  }
  return normalizePaneWidthPx(Math.min(ratio, MAX_LEGACY_PANE_RATIO) * getMinPaneWidth());
}

function persistedWidthFor(record: Record<string, unknown>, version: unknown): number {
  return version === PANE_LAYOUT_SETTINGS_VERSION
    ? normalizePersistedWidthPx(record.widthPx)
    : migrateLegacyRatioToWidthPx(record.ratio);
}

function isSafePersistedPaneId(paneId: string): boolean {
  return paneId.length > 0 &&
    paneId.length <= MAX_PANE_ID_LENGTH &&
    SAFE_PANE_ID_PATTERN.test(paneId);
}

// A THREAD pane whose persisted id is shaped like a companion id would
// collide with the deterministic id minted when that companion later opens
// for a same-named source pane (companionPaneIdFor('main','plan') ===
// 'plan-main'), so reject it at the parse edge. Real companion entries
// keep the shape — this guard applies to thread panes only.
const COMPANION_SHAPED_PANE_ID = /^(?:plan|design-preview|review|take-control)-/;

function isSafePersistedThreadPaneId(paneId: string): boolean {
  return isSafePersistedPaneId(paneId) && !COMPANION_SHAPED_PANE_ID.test(paneId);
}

function isSafePersistedThreadId(threadId: string): boolean {
  return threadId.length > 0 && threadId.length <= MAX_THREAD_ID_LENGTH;
}

// take-control is deliberately absent: a live PTY mirror is never persisted.
function isPersistedCompanionKind(kind: unknown): kind is CompanionPanelKind {
  return kind === 'plan' || kind === 'design-preview' || kind === 'review';
}

function companionPaneIdFor(sourcePaneId: string, kind: CompanionPanelKind): string {
  return `${kind}-${sourcePaneId}`;
}

async function emptyLayout(): Promise<void> {
  setPaneLayoutItems([]);
  resetPaneRegistry(null);
  resetWorkflowsPane();
}

function parsePersistedLayout(raw: unknown): PersistedPaneLayout | null {
  if (!raw || typeof raw !== 'object') return null;
  const record = raw as Record<string, unknown>;
  if (record.version !== 1 && record.version !== 2 && record.version !== PANE_LAYOUT_SETTINGS_VERSION) {
    return null;
  }
  if (!Array.isArray(record.panes)) return null;
  const focusedPaneId = typeof record.focusedPaneId === 'string' && isSafePersistedPaneId(record.focusedPaneId)
    ? record.focusedPaneId
    : null;
  const panes: PersistedPane[] = [];
  if (record.version === 1) {
    const seenPaneIds = new Set<string>();
    const seenThreadIds = new Set<string>();
    for (const item of record.panes) {
      if (!item || typeof item !== 'object') continue;
      const pane = item as Record<string, unknown>;
      if (typeof pane.paneId !== 'string' || !isSafePersistedThreadPaneId(pane.paneId)) continue;
      if (typeof pane.threadId !== 'string' || !isSafePersistedThreadId(pane.threadId)) continue;
      if (seenPaneIds.has(pane.paneId) || seenThreadIds.has(pane.threadId)) continue;
      seenPaneIds.add(pane.paneId);
      seenThreadIds.add(pane.threadId);
      panes.push({
        paneId: pane.paneId,
        kind: 'thread',
        threadId: pane.threadId,
        widthPx: migrateLegacyRatioToWidthPx(pane.ratio),
      });
      if (panes.length >= MAX_RESTORED_PANES) break;
    }
    return { version: PANE_LAYOUT_SETTINGS_VERSION, panes, focusedPaneId };
  }

  const validThreadPaneIds = new Set<string>();
  const firstPassPaneIds = new Set<string>();
  const firstPassThreadIds = new Set<string>();
  for (const item of record.panes) {
    if (!item || typeof item !== 'object') continue;
    const pane = item as Record<string, unknown>;
    if (pane.kind !== 'thread') continue;
    if (typeof pane.paneId !== 'string' || !isSafePersistedThreadPaneId(pane.paneId)) continue;
    if (typeof pane.threadId !== 'string' || !isSafePersistedThreadId(pane.threadId)) continue;
    if (firstPassPaneIds.has(pane.paneId) || firstPassThreadIds.has(pane.threadId)) continue;
    firstPassPaneIds.add(pane.paneId);
    firstPassThreadIds.add(pane.threadId);
    validThreadPaneIds.add(pane.paneId);
  }

  const seenPaneIds = new Set<string>();
  const seenThreadIds = new Set<string>();
  for (const item of record.panes) {
    if (!item || typeof item !== 'object') continue;
    const pane = item as Record<string, unknown>;
    if (typeof pane.paneId !== 'string' || !isSafePersistedPaneId(pane.paneId)) continue;
    if (seenPaneIds.has(pane.paneId)) continue;
    if (pane.kind === 'thread') {
      if (!isSafePersistedThreadPaneId(pane.paneId)) continue;
      if (typeof pane.threadId !== 'string' || !isSafePersistedThreadId(pane.threadId)) continue;
      if (seenThreadIds.has(pane.threadId)) continue;
      seenPaneIds.add(pane.paneId);
      seenThreadIds.add(pane.threadId);
      panes.push({
        paneId: pane.paneId,
        kind: 'thread',
        threadId: pane.threadId,
        widthPx: persistedWidthFor(pane, record.version),
      });
    } else if (pane.kind === 'workflows') {
      if (pane.paneId !== 'workflows') continue;
      if (!parsePersistedWorkflowsPaneState(pane.workflowState)) continue;
      seenPaneIds.add(pane.paneId);
      panes.push({
        paneId: pane.paneId,
        kind: 'workflows',
        workflowState: pane.workflowState,
        widthPx: persistedWidthFor(pane, record.version),
      });
    } else if (isPersistedCompanionKind(pane.kind)) {
      if (typeof pane.sourcePaneId !== 'string' || !isSafePersistedPaneId(pane.sourcePaneId)) continue;
      if (!validThreadPaneIds.has(pane.sourcePaneId)) continue;
      if (pane.paneId !== companionPaneIdFor(pane.sourcePaneId, pane.kind)) continue;
      seenPaneIds.add(pane.paneId);
      panes.push({
        paneId: pane.paneId,
        kind: pane.kind,
        sourcePaneId: pane.sourcePaneId,
        widthPx: persistedWidthFor(pane, record.version),
      });
    }
    if (panes.length >= MAX_RESTORED_PANES) break;
  }
  return { version: PANE_LAYOUT_SETTINGS_VERSION, panes, focusedPaneId };
}

function paneLayoutKey(snapshot: PersistedPaneLayout): string {
  return JSON.stringify(snapshot);
}

function buildSnapshot(): PersistedPaneLayout {
  const panesById = getAllPanes();
  const layoutItems = getPaneLayoutItems();
  const persistedThreadPaneIds = new Set<string>();
  for (const item of layoutItems) {
    if (item.kind !== 'thread') continue;
    if (panesById.get(item.paneId)?.threadId) persistedThreadPaneIds.add(item.paneId);
  }
  const panes: PersistedPane[] = [];
  for (const item of layoutItems) {
    if (item.kind === 'thread') {
      const threadId = panesById.get(item.paneId)?.threadId;
      if (!threadId) continue;
      panes.push({
        paneId: item.paneId,
        kind: 'thread',
        threadId,
        widthPx: normalizePersistedWidthPx(item.widthPx),
      });
    } else if (item.kind === 'workflows') {
      panes.push({
        paneId: item.paneId,
        kind: 'workflows',
        workflowState: getPersistedWorkflowsPaneState(),
        widthPx: normalizePersistedWidthPx(item.widthPx),
      });
    } else if (
      isPersistedCompanionKind(item.kind) &&
      item.sourcePaneId &&
      persistedThreadPaneIds.has(item.sourcePaneId)
    ) {
      panes.push({
        paneId: item.paneId,
        kind: item.kind,
        sourcePaneId: item.sourcePaneId,
        widthPx: normalizePersistedWidthPx(item.widthPx),
      });
    }
  }
  // Focus persists as-is when the focused pane itself is in the snapshot
  // (thread panes and panel companions). A focused take-control pane is
  // ephemeral — fall back to its source thread pane, then to the first pane.
  const focusedPaneId = getFocusedPaneId();
  const focusedThreadPaneId = getFocusedThreadPaneId();
  const persistableFocusId = [focusedPaneId, focusedThreadPaneId].find(
    (candidate) => candidate && panes.some((pane) => pane.paneId === candidate),
  ) ?? null;
  return {
    version: PANE_LAYOUT_SETTINGS_VERSION,
    panes,
    focusedPaneId: persistableFocusId ?? panes[0]?.paneId ?? null,
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
  const threadPanes = persisted.panes.filter((pane) => pane.kind === 'thread' && pane.threadId);
  const workflowsPane = persisted.panes.find((pane) => pane.kind === 'workflows');
  const companionPanes = persisted.panes.filter((pane) => isPersistedCompanionKind(pane.kind));
  const neededThreadIds = new Set(threadPanes.map((pane) => pane.threadId as string));
  const threadById = new Map<string, Thread>();
  for (const thread of threads) {
    const threadId = thread.id;
    if (neededThreadIds.has(threadId)) threadById.set(threadId, thread);
  }
  const layoutItems: PaneLayoutItem[] = [];
  const registryEntries: Array<{ paneId: string; thread: Thread }> = [];
  for (const pane of threadPanes) {
    const thread = threadById.get(pane.threadId as string);
    if (!thread) continue;
    layoutItems.push({
      id: pane.paneId,
      paneId: pane.paneId,
      kind: 'thread',
      widthPx: pane.widthPx,
    });
    registryEntries.push({ paneId: pane.paneId, thread });
  }

  if (workflowsPane) {
    const threadItemsById = new Map(layoutItems.map((item) => [item.paneId, item]));
    layoutItems.length = 0;
    for (const pane of persisted.panes) {
      if (pane.kind === 'thread') {
        const item = threadItemsById.get(pane.paneId);
        if (item) layoutItems.push(item);
      } else if (pane.kind === 'workflows') {
        layoutItems.push({
          id: pane.paneId, paneId: pane.paneId, kind: 'workflows', widthPx: pane.widthPx,
        });
      }
    }
  }

  const restoredFocusedPaneId = persisted.focusedPaneId &&
    registryEntries.some((entry) => entry.paneId === persisted.focusedPaneId)
    ? persisted.focusedPaneId
    : registryEntries[0]?.paneId ?? null;
  setPaneLayoutItems(layoutItems);
  await hydrateRestoredPaneRegistry(registryEntries, restoredFocusedPaneId);

  const restoredThreadItems = layoutItems.filter(
    (item) => item.kind === 'workflows' || getAllPanes().has(item.paneId),
  );
  const companionsBySource = new Map<string, PaneLayoutItem[]>();
  for (const pane of companionPanes) {
    if (!isPersistedCompanionKind(pane.kind) || !pane.sourcePaneId) continue;
    const sourcePane = getAllPanes().get(pane.sourcePaneId);
    if (!sourcePane) continue;
    if (pane.kind === 'design-preview' && sourcePane.thread?.mode !== 'design') continue;
    const item: PaneLayoutItem = {
      id: pane.paneId,
      paneId: pane.paneId,
      kind: pane.kind,
      widthPx: pane.widthPx,
      sourcePaneId: pane.sourcePaneId,
    };
    const companions = companionsBySource.get(pane.sourcePaneId) ?? [];
    companions.push(item);
    companionsBySource.set(pane.sourcePaneId, companions);
    restoreCompanion(pane.sourcePaneId, pane.kind, pane.paneId);
  }
  const restoredLayoutItems: PaneLayoutItem[] = [];
  for (const item of restoredThreadItems) {
    restoredLayoutItems.push(item);
    const companions = companionsBySource.get(item.paneId);
    if (companions) restoredLayoutItems.push(...companions);
  }
  setPaneLayoutItems(restoredLayoutItems);

  if (workflowsPane) {
    await restoreWorkflowsPaneState(parsePersistedWorkflowsPaneState(workflowsPane.workflowState));
  }

  // A persisted focused COMPANION id can only be honored now — the hydrate
  // above validated against thread panes and fell back to one of those.
  // Upgrade to the companion once its layout item exists.
  if (
    persisted.focusedPaneId &&
    persisted.focusedPaneId !== getFocusedPaneId() &&
    restoredLayoutItems.some((item) => item.paneId === persisted.focusedPaneId)
  ) {
    focusPane(persisted.focusedPaneId);
  }
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
  setWorkflowsPanePersistenceHandler(persistPaneLayout);
}

export async function waitForPaneLayoutPersistenceForTest(): Promise<void> {
  await flushAppStorage();
}

export function resetPaneLayoutPersistenceForTest(): void {
  persistPaneLayoutDebounced.cancel();
  lastPersistedPaneLayoutKey = null;
  setPaneLayoutPersistenceHandlers(null);
  setPanePersistenceHandler(null);
  setWorkflowsPanePersistenceHandler(null);
}

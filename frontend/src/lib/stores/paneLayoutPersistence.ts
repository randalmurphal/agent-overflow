import type { Thread } from '../types/models';
import type {
  AgentPaneBreadcrumbEntry,
  AgentPaneScopeSnapshot,
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
import { restoreCompanion, type PersistedCompanionKind } from './companionPanes.svelte';
import { agentScopeForPane, seedAgentStateForPane } from './agentPane.svelte';
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
// Deliberately NOT bumped for the agent companion's `agentScope` field.
// The accepted-version check below rejects the WHOLE layout on an unknown
// version, so a bump would make an older build throw away every pane rather
// than the one it cannot render. An unknown `kind` degrades far better:
// `isPersistedCompanionKind` skips the agent entry and every other pane
// restores. New optional fields on an existing version are read tolerantly
// in both directions, which is the property that makes the bump unnecessary.
const PANE_LAYOUT_SETTINGS_VERSION = 3;
const PANE_RESIZE_PERSIST_DELAY_MS = 200;
const MAX_RESTORED_PANES = 24;
const MAX_PANE_ID_LENGTH = 64;
const MAX_THREAD_ID_LENGTH = 256;
const MAX_ITEM_ID_LENGTH = 256;
const MAX_BREADCRUMB_ENTRIES = 32;
const MAX_BREADCRUMB_LABEL_LENGTH = 200;
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
const COMPANION_SHAPED_PANE_ID = /^(?:plan|review|take-control|browser|agent)-/;

function isSafePersistedThreadPaneId(paneId: string): boolean {
  return isSafePersistedPaneId(paneId) && !COMPANION_SHAPED_PANE_ID.test(paneId);
}

function isSafePersistedThreadId(threadId: string): boolean {
  return threadId.length > 0 && threadId.length <= MAX_THREAD_ID_LENGTH;
}

// Live PTY/browser surfaces are deliberately absent: neither can be restored.
function isPersistedCompanionKind(kind: unknown): kind is PersistedCompanionKind {
  return kind === 'plan' || kind === 'review' || kind === 'agent';
}

/**
 * Validate an agent companion's persisted scope. Anything short of a
 * well-formed trail ending at a non-empty scope id answers null, and the
 * caller drops the PANE — an agent pane with no scope has nothing to render,
 * and guessing a scope would open a reader onto the wrong node's transcript.
 *
 * The scoped ROW is deliberately not required here: items load after the
 * layout does, so restore cannot know yet whether the row survives. The pane
 * body self-closes when it resolves the scope to nothing (spec Q5).
 */
function parsePersistedAgentScope(value: unknown): AgentPaneScopeSnapshot | null {
  if (!value || typeof value !== 'object') return null;
  const record = value as Record<string, unknown>;
  const scopeItemId = record.scopeItemId;
  if (typeof scopeItemId !== 'string') return null;
  if (scopeItemId.length === 0 || scopeItemId.length > MAX_ITEM_ID_LENGTH) return null;
  if (!Array.isArray(record.breadcrumb)) return null;
  if (record.breadcrumb.length === 0 || record.breadcrumb.length > MAX_BREADCRUMB_ENTRIES) return null;
  const breadcrumb: AgentPaneBreadcrumbEntry[] = [];
  for (const entry of record.breadcrumb) {
    if (!entry || typeof entry !== 'object') return null;
    const hop = entry as Record<string, unknown>;
    // The root hop is the thread itself and carries an empty itemId; every
    // other hop names a launch row.
    if (typeof hop.itemId !== 'string' || hop.itemId.length > MAX_ITEM_ID_LENGTH) return null;
    if (typeof hop.label !== 'string') return null;
    breadcrumb.push({ itemId: hop.itemId, label: hop.label.slice(0, MAX_BREADCRUMB_LABEL_LENGTH) });
  }
  // The trail must END at the scope, or the breadcrumb would describe an
  // ancestry the pane is not actually showing.
  if (breadcrumb[breadcrumb.length - 1].itemId !== scopeItemId) return null;
  return { scopeItemId, breadcrumb };
}

function companionPaneIdFor(sourcePaneId: string, kind: PersistedCompanionKind): string {
  return `${kind}-${sourcePaneId}`;
}

function emptyLayout(): void {
  setPaneLayoutItems([]);
  resetPaneRegistry(null);
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
    } else if (isPersistedCompanionKind(pane.kind)) {
      if (typeof pane.sourcePaneId !== 'string' || !isSafePersistedPaneId(pane.sourcePaneId)) continue;
      if (!validThreadPaneIds.has(pane.sourcePaneId)) continue;
      if (pane.paneId !== companionPaneIdFor(pane.sourcePaneId, pane.kind)) continue;
      // An agent pane IS its scope: no scope, no pane.
      const agentScope = pane.kind === 'agent' ? parsePersistedAgentScope(pane.agentScope) : null;
      if (pane.kind === 'agent' && !agentScope) continue;
      seenPaneIds.add(pane.paneId);
      const restored: PersistedPane = {
        paneId: pane.paneId,
        kind: pane.kind,
        sourcePaneId: pane.sourcePaneId,
        widthPx: persistedWidthFor(pane, record.version),
      };
      if (agentScope) restored.agentScope = agentScope;
      panes.push(restored);
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
    } else if (
      isPersistedCompanionKind(item.kind) &&
      item.sourcePaneId &&
      persistedThreadPaneIds.has(item.sourcePaneId)
    ) {
      const entry: PersistedPane = {
        paneId: item.paneId,
        kind: item.kind,
        sourcePaneId: item.sourcePaneId,
        widthPx: normalizePersistedWidthPx(item.widthPx),
      };
      if (item.kind === 'agent') {
        // Live scope first; the layout item's carried copy covers the window
        // between a restore and the body's first mount. A scope neither can
        // answer for means the pane is not restorable — drop the ENTRY, not
        // just the field, so a reload doesn't resurrect a blank agent pane.
        const threadId = panesById.get(item.sourcePaneId)?.threadId ?? '';
        const scope = agentScopeForPane(item.sourcePaneId, threadId) ?? item.agentScope ?? null;
        if (!scope || !scope.scopeItemId) continue;
        entry.agentScope = {
          scopeItemId: scope.scopeItemId,
          breadcrumb: scope.breadcrumb.map((hop) => ({ ...hop })),
        };
      }
      panes.push(entry);
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
    emptyLayout();
    return;
  }

  const threads = await loadThreadsForValidation(availableThreads);
  const threadPanes = persisted.panes.filter((pane) => pane.kind === 'thread' && pane.threadId);
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

  const restoredFocusedPaneId = persisted.focusedPaneId &&
    registryEntries.some((entry) => entry.paneId === persisted.focusedPaneId)
    ? persisted.focusedPaneId
    : registryEntries[0]?.paneId ?? null;
  setPaneLayoutItems(layoutItems);
  await hydrateRestoredPaneRegistry(registryEntries, restoredFocusedPaneId);

  const restoredThreadItems = layoutItems.filter((item) => getAllPanes().has(item.paneId));
  const companionsBySource = new Map<string, PaneLayoutItem[]>();
  for (const pane of companionPanes) {
    if (!isPersistedCompanionKind(pane.kind) || !pane.sourcePaneId) continue;
    const sourcePane = getAllPanes().get(pane.sourcePaneId);
    if (!sourcePane) continue;
    // The agent pane's scope is seeded from the snapshot, keyed by the SOURCE
    // pane (one agent pane per source pane). A source pane that failed to
    // hydrate a thread has nothing to scope against, so drop the companion —
    const sourceThreadId = sourcePane.threadId;
    if (pane.kind === 'agent' && (!pane.agentScope || !sourceThreadId)) continue;
    const item: PaneLayoutItem = {
      id: pane.paneId,
      paneId: pane.paneId,
      kind: pane.kind,
      widthPx: pane.widthPx,
      sourcePaneId: pane.sourcePaneId,
    };
    if (pane.kind === 'agent' && pane.agentScope && sourceThreadId) {
      item.agentScope = pane.agentScope;
      seedAgentStateForPane(pane.sourcePaneId, sourceThreadId, pane.agentScope);
    }
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

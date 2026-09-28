// Companions belong to their thread, per client.
//
// When a pane leaves a thread (switches away, closes, starts a draft), the
// companions open on it are recorded here under the thread's id and taken
// down. The next time any pane mounts that thread they reopen as they were,
// at their widths and in their order; an agent pane keeps its scope and
// breadcrumb, and a side chat its conversation. Closing a companion yourself
// forgets it. Deleting or archiving the thread forgets all of them and
// deletes any side chat still hidden with it.
//
// Plan, review and agent entries persist in this client's appStorage: the
// kinds the pane layout persists. take-control and side-chat entries live
// for the session, as their panes do, and a side chat's thread is swept at
// boot. Browser is never recorded: its visibility is the backend's, and
// browserCompanion.svelte.ts reopens it when the thread mounts.

import type { Thread } from '../types/models';
import type { AgentPaneScopeSnapshot } from '../types/settings';
import { TransportError } from '../transport/wsClient';
import { normalizePaneWidthPx } from '../utils/paneWidths';
import { userFacingError } from '../utils/userFacingError';
import { agentScopeForPane, seedAgentStateForPane } from './agentPane.svelte';
import { appStorageDelete, appStorageGet, appStorageSet } from './appStorage';
import { GetThread } from './bindings';
import {
  closeCompanion,
  closeCompanionsForSource,
  companionsForSource,
  hideCompanion,
  openCompanion,
  type CompanionKind,
  type CompanionPaneState,
} from './companionPanes.svelte';
import { isDraftPlaceholderId } from './draftPlaceholderId';
import { getPaneLayoutItems, type PaneLayoutItem } from './paneLayout.svelte';
import { parsePersistedAgentScope } from './paneLayoutPersistence';
import {
  addPaneThreadMountedObserver,
  createPane,
  getAllPanes,
  getPane,
  mountThreadInPane,
} from './panes.svelte';
import { deleteSideChatThread } from './sideChatThread';
import { addThreadRemovedObserver } from './threads.svelte';
import { addToast } from './toast.svelte';

type StashedKind = Exclude<CompanionKind, 'browser'>;
type PersistedStashedKind = 'plan' | 'review' | 'agent';

interface StashedCompanion {
  kind: StashedKind;
  widthPx: number;
  /** agent: the scope and breadcrumb it was showing. */
  agentScope?: AgentPaneScopeSnapshot;
  /** side-chat: the scratch thread it hosts. */
  thread?: Thread;
}

const stashByThread = new Map<string, StashedCompanion[]>();
// Side chats being reopened, by pane id. The pane has no thread until its
// mount settles, and leaving again before then must still record the chat.
const reopeningSideChats = new Map<string, Thread>();
let uninstall: (() => void) | null = null;

function storageKey(threadId: string): string {
  return `companionStash:${threadId}`;
}

function isPersistedStashedKind(kind: unknown): kind is PersistedStashedKind {
  return kind === 'plan' || kind === 'review' || kind === 'agent';
}

/**
 * Hide every companion paired to `sourcePaneId` and remember them for
 * `thread`, the thread the pane is leaving. A draft placeholder is never
 * mounted again under its id, so its companions close instead.
 */
export function stashCompanions(sourcePaneId: string, thread: Thread | null): void {
  const companions = companionsForSource(sourcePaneId);
  if (companions.length === 0) return;
  if (!thread || isDraftPlaceholderId(thread.id)) {
    closeCompanionsForSource(sourcePaneId);
    return;
  }
  const itemsById = new Map(getPaneLayoutItems().map((item) => [item.paneId, item]));
  const stashed: StashedCompanion[] = [];
  for (const companion of companions) {
    const entry = stashEntry(companion, thread.id, itemsById.get(companion.paneId));
    if (!entry) {
      closeCompanion(companion.paneId);
      continue;
    }
    stashed.push(entry);
    hideCompanion(companion.paneId);
  }
  writeStash(thread, stashed);
}

function stashEntry(
  companion: CompanionPaneState,
  threadId: string,
  item: PaneLayoutItem | undefined,
): StashedCompanion | null {
  const widthPx = item?.widthPx ?? 0;
  switch (companion.kind) {
    case 'browser':
      return null;
    case 'agent': {
      // Live scope first; the layout item carries a restored scope until the
      // pane body first mounts. A pane with neither has nothing to reopen.
      const agentScope = agentScopeForPane(companion.sourcePaneId, threadId) ?? item?.agentScope;
      return agentScope ? { kind: 'agent', widthPx, agentScope } : null;
    }
    case 'side-chat': {
      // A side chat still being forked has no thread to keep; closing it lets
      // openSideChat delete the fork when it lands.
      const thread = getPane(companion.paneId)?.thread ?? reopeningSideChats.get(companion.paneId);
      return thread ? { kind: 'side-chat', widthPx, thread } : null;
    }
    default:
      return { kind: companion.kind, widthPx };
  }
}

function writeStash(thread: Thread, entries: StashedCompanion[]): void {
  if (entries.length === 0) {
    dropStash(thread.id);
    return;
  }
  stashByThread.set(thread.id, entries);
  const persisted = thread.mode === 'scratch'
    ? []
    : entries.filter((entry) => isPersistedStashedKind(entry.kind));
  if (persisted.length === 0) {
    deleteStoredStash(thread.id);
    return;
  }
  appStorageSet(storageKey(thread.id), JSON.stringify(persisted.map((entry) => ({
    kind: entry.kind,
    widthPx: entry.widthPx,
    ...(entry.agentScope ? { agentScope: entry.agentScope } : {}),
  }))));
}

function deleteStoredStash(threadId: string): void {
  if (appStorageGet(storageKey(threadId)) !== null) appStorageDelete(storageKey(threadId));
}

function dropStash(threadId: string): void {
  stashByThread.delete(threadId);
  deleteStoredStash(threadId);
}

// The session's entry when there is one, since it also holds the session-only
// kinds; otherwise what an earlier session persisted.
function readStash(threadId: string): StashedCompanion[] | null {
  const live = stashByThread.get(threadId);
  if (live) return live;
  const raw = appStorageGet(storageKey(threadId));
  if (raw === null) return null;
  return parseStoredStash(raw);
}

function parseStoredStash(raw: string): StashedCompanion[] {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return [];
  }
  if (!Array.isArray(parsed)) return [];
  const entries: StashedCompanion[] = [];
  for (const value of parsed) {
    if (!value || typeof value !== 'object') continue;
    const record = value as Record<string, unknown>;
    if (!isPersistedStashedKind(record.kind)) continue;
    const widthPx = typeof record.widthPx === 'number' ? normalizePaneWidthPx(record.widthPx) : 0;
    if (record.kind === 'agent') {
      const agentScope = parsePersistedAgentScope(record.agentScope);
      if (agentScope) entries.push({ kind: 'agent', widthPx, agentScope });
      continue;
    }
    entries.push({ kind: record.kind, widthPx });
  }
  return entries;
}

/** Reopen what `threadId` had open when a pane last left it. */
function reopenStashedCompanions(paneId: string, threadId: string): void {
  // The mount can settle after the pane has already moved on or gone; the
  // stash then waits for the next mount.
  if (getPane(paneId)?.threadId !== threadId) return;
  const entries = readStash(threadId);
  if (!entries) return;
  dropStash(threadId);
  for (const entry of entries) {
    const placement = { widthPx: entry.widthPx || undefined, reveal: false };
    switch (entry.kind) {
      case 'agent':
        if (!entry.agentScope) break;
        seedAgentStateForPane(paneId, threadId, entry.agentScope);
        openCompanion(paneId, 'agent', placement);
        break;
      case 'side-chat':
        if (entry.thread) void reopenSideChat(paneId, threadId, entry.thread, placement.widthPx);
        break;
      default:
        openCompanion(paneId, entry.kind, placement);
    }
  }
}

async function reopenSideChat(
  sourcePaneId: string,
  sourceThreadId: string,
  stashedThread: Thread,
  widthPx: number | undefined,
): Promise<void> {
  // The slot opens now so the side chat keeps its place among its siblings;
  // it shows as preparing until its thread is confirmed and mounted.
  const companion = openCompanion(sourcePaneId, 'side-chat', { widthPx, reveal: false });
  if (!companion) {
    void deleteSideChatThread(stashedThread.id);
    return;
  }
  const pane = createPane(companion.paneId);
  const stillOwnsPane = () => getPane(companion.paneId) === pane;
  reopeningSideChats.set(companion.paneId, stashedThread);
  try {
    let thread: Thread;
    try {
      thread = (await GetThread(stashedThread.id)) as Thread;
    } catch (err) {
      // Its computer restarted and swept it: there is nothing left to reopen.
      if (err instanceof TransportError && err.code === 'not_found') {
        if (stillOwnsPane()) closeCompanion(companion.paneId);
        dropStash(stashedThread.id);
        return;
      }
      throw err;
    }
    // Left again while the read was out: the stash recorded it from
    // reopeningSideChats.
    if (!stillOwnsPane()) return;
    await mountThreadInPane(thread, pane, 'committed', { background: true });
  } catch (err) {
    console.error('Failed to reopen the side chat:', err);
    // Keep the conversation for the next visit rather than delete it over a
    // failed read.
    if (stillOwnsPane()) {
      hideCompanion(companion.paneId);
      const entries = stashByThread.get(sourceThreadId) ?? [];
      stashByThread.set(sourceThreadId, [
        ...entries,
        { kind: 'side-chat', widthPx: widthPx ?? 0, thread: stashedThread },
      ]);
    }
    addToast(
      'error',
      `The side chat could not be reopened (${userFacingError(err)}). It will try again when you return to this thread.`,
    );
  } finally {
    if (reopeningSideChats.get(companion.paneId) === stashedThread) {
      reopeningSideChats.delete(companion.paneId);
    }
  }
}

// A deleted or archived thread takes its companions with it: those open on a
// pane still showing it close for good, and what was hidden for it is
// forgotten, side chats deleted. A deleted side chat also leaves the stash of
// the thread it was hidden with.
function onThreadRemoved(threadId: string): void {
  for (const pane of getAllPanes().values()) {
    if (pane.threadId === threadId) closeCompanionsForSource(pane.paneId);
  }
  const entries = stashByThread.get(threadId) ?? [];
  dropStash(threadId);
  for (const entry of entries) {
    if (entry.thread) void deleteSideChatThread(entry.thread.id);
  }
  for (const [ownerId, ownerEntries] of stashByThread) {
    const kept = ownerEntries.filter((entry) => entry.thread?.id !== threadId);
    if (kept.length === ownerEntries.length) continue;
    if (kept.length > 0) stashByThread.set(ownerId, kept);
    else stashByThread.delete(ownerId);
  }
}

export function installCompanionStash(): void {
  uninstall?.();
  const offMounted = addPaneThreadMountedObserver(reopenStashedCompanions);
  const offRemoved = addThreadRemovedObserver(onThreadRemoved);
  uninstall = () => {
    offMounted();
    offRemoved();
  };
}

export function resetCompanionStashForTest(): void {
  uninstall?.();
  uninstall = null;
  stashByThread.clear();
  reopeningSideChats.clear();
}

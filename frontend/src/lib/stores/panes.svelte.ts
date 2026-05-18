import type { Thread } from '../types/models';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import {
  addPaneLayoutItem,
  averagePaneRatio,
  getPaneLayoutItems,
  movePaneLayoutItem,
  removePaneLayoutItem,
} from './paneLayout.svelte';
import { touchProjectActivity } from './projects.svelte';
import { replaceThread as replaceThreadInRegistry } from './threads.svelte';

// Active panes, keyed by pane ID. PaneHost mounts panes from layout order;
// command routing and sidebar actions resolve explicit pane targets through
// this registry.
let panes: Map<string, ThreadPane> = $state(new Map());
let focusedPaneId: string | null = $state('main');
export type PaneActivation = 'preview' | 'committed';
let paneActivationById: Map<string, PaneActivation> = $state(new Map());
let nextGeneratedPaneId = 1;

function requestPaneReveal(paneId: string): void {
  if (typeof window === 'undefined' || !paneId) return;
  window.dispatchEvent(new CustomEvent('agent-overflow:reveal-pane', {
    detail: { paneId },
  }));
}

function hasLayoutPane(paneId: string): boolean {
  return getPaneLayoutItems().some((item) => item.paneId === paneId);
}

function addThreadPaneToLayout(paneId: string, insertIndex?: number): void {
  if (hasLayoutPane(paneId)) return;
  addPaneLayoutItem({
    id: paneId,
    paneId,
    kind: 'thread',
    ratio: averagePaneRatio(),
  }, insertIndex);
}

function nextPaneId(): string {
  if (panes.size === 0) return 'main';
  let id = `pane-${nextGeneratedPaneId}`;
  while (panes.has(id) || hasLayoutPane(id)) {
    nextGeneratedPaneId += 1;
    id = `pane-${nextGeneratedPaneId}`;
  }
  nextGeneratedPaneId += 1;
  return id;
}

function orderedPaneIds(): string[] {
  return getPaneLayoutItems()
    .map((item) => item.paneId)
    .filter((paneId) => panes.has(paneId));
}

function registerPane(id: string, pane: ThreadPane, activation: PaneActivation = 'committed'): ThreadPane {
  panes = new Map(panes).set(id, pane);
  paneActivationById = new Map(paneActivationById).set(id, activation);
  return pane;
}

export function ensureMainPane(): ThreadPane {
  let main = panes.get('main');
  if (!main) {
    main = createThreadPane({ paneId: 'main' });
    registerPane('main', main, 'committed');
  }
  return main;
}

export function getMainPane(): ThreadPane {
  const main = panes.get('main');
  if (!main) {
    throw new Error('Pane registry is missing the main pane.');
  }
  return main;
}

export function createPane(id: string): ThreadPane {
  const existing = panes.get(id);
  if (existing) return existing;
  const pane = createThreadPane({ paneId: id });
  return registerPane(id, pane, 'committed');
}

export function getPane(id: string): ThreadPane | undefined {
  return panes.get(id);
}

export function registerPaneForTest(id: string, pane: ThreadPane): void {
  registerPane(id, pane, 'committed');
}

export function getFocusedPane(): ThreadPane {
  const pane = getFocusedPaneOrNull();
  if (!pane) {
    throw new Error(`Focused pane "${focusedPaneId ?? 'none'}" is not registered.`);
  }
  return pane;
}

export function getFocusedPaneOrNull(): ThreadPane | null {
  return focusedPaneId ? panes.get(focusedPaneId) ?? null : null;
}

export function focusPane(id: string): void {
  if (focusedPaneId === id) return;
  if (!panes.has(id)) return;
  focusedPaneId = id;
  requestPaneReveal(id);
}

export function getFocusedPaneId(): string | null {
  return focusedPaneId;
}

export function getAllPanes(): Map<string, ThreadPane> {
  return panes;
}

export function listPanes(): ThreadPane[] {
  return Array.from(panes.values());
}

export function iterPanes(): IterableIterator<ThreadPane> {
  return panes.values();
}

export function forEachPane(fn: (pane: ThreadPane) => void): void {
  for (const pane of panes.values()) {
    fn(pane);
  }
}

export function panesShowingThread(threadId: string): ThreadPane[] {
  return listPanes().filter((pane) => pane.threadId === threadId);
}

export function forPanesShowingThread(
  threadId: string,
  fn: (pane: ThreadPane) => void,
): void {
  for (const pane of panes.values()) {
    if (pane.threadId !== threadId) continue;
    fn(pane);
  }
}

export function isThreadVisible(threadId: string): boolean {
  return findPaneShowingThread(threadId) !== null;
}

export function destroyPane(id: string): void {
  const pane = panes.get(id);
  if (!pane) return;
  const order = orderedPaneIds();
  const removedIndex = order.indexOf(id);
  const nextFocusId = removedIndex > 0
    ? order[removedIndex - 1]
    : order.find((paneId) => paneId !== id) ?? null;
  pane.clear();
  panes = new Map(panes);
  panes.delete(id);
  paneActivationById = new Map(paneActivationById);
  paneActivationById.delete(id);
  removePaneLayoutItem(id);
  if (focusedPaneId !== id) return;
  focusedPaneId = nextFocusId;
  if (nextFocusId) requestPaneReveal(nextFocusId);
}

export function clearPanesShowingThread(threadId: string): void {
  for (const pane of panes.values()) {
    if (pane.threadId === threadId) pane.clear();
  }
}

export function clearPanesShowingThreads(threadIds: Iterable<string>): void {
  const ids = new Set(threadIds);
  if (ids.size === 0) return;
  for (const pane of panes.values()) {
    if (pane.threadId && ids.has(pane.threadId)) pane.clear();
  }
}

export function closeFocusedPane(): void {
  const pane = getFocusedPaneOrNull();
  if (!pane) return;
  destroyPane(pane.paneId);
}

export function getPaneActivation(id: string): PaneActivation {
  return paneActivationById.get(id) ?? 'committed';
}

export function commitPanePreview(id: string): void {
  if (!panes.has(id)) return;
  if (paneActivationById.get(id) === 'committed') return;
  paneActivationById = new Map(paneActivationById).set(id, 'committed');
}

export function resetPanesForTest(): void {
  for (const pane of panes.values()) pane.clear();
  panes = new Map();
  paneActivationById = new Map();
  focusedPaneId = 'main';
  nextGeneratedPaneId = 1;
}

export function findPaneShowingThread(threadId: string): ThreadPane | null {
  for (const pane of panes.values()) {
    if (pane.threadId !== threadId) continue;
    return pane;
  }
  return null;
}

export async function replaceThreadInPane(
  thread: Thread,
  targetPane: string | ThreadPane,
  activation: PaneActivation = 'committed',
): Promise<ThreadPane> {
  const target = typeof targetPane === 'string'
    ? panes.get(targetPane)
    : targetPane;
  if (!target) {
    throw new Error(`Target pane "${targetPane}" is not registered.`);
  }
  if (!panes.has(target.paneId)) {
    registerPane(target.paneId, target, activation);
  } else {
    paneActivationById = new Map(paneActivationById).set(target.paneId, activation);
  }
  addThreadPaneToLayout(target.paneId);
  focusedPaneId = target.paneId;
  requestPaneReveal(target.paneId);
  await target.switchThread(thread);
  return target;
}

export async function revealThreadIfOpen(thread: Thread): Promise<ThreadPane | null> {
  const pane = findPaneShowingThread(thread.id);
  if (!pane) return null;
  focusedPaneId = pane.paneId;
  requestPaneReveal(pane.paneId);
  return pane;
}

function resolveOpenTargetPane(targetPane?: string | ThreadPane | null): string | ThreadPane {
  if (targetPane) return targetPane;
  const focused = getFocusedPaneOrNull();
  if (focused) return focused;
  return ensureMainPane();
}

export async function openThreadInPane(
  thread: Thread,
  targetPane?: string | ThreadPane | null,
): Promise<ThreadPane> {
  const existing = await revealThreadIfOpen(thread);
  if (existing) {
    commitPanePreview(existing.paneId);
    return existing;
  }
  return replaceThreadInPane(thread, resolveOpenTargetPane(targetPane), 'committed');
}

export async function openThreadFromNavigation(
  thread: Thread,
  targetPane?: string | ThreadPane | null,
): Promise<ThreadPane> {
  const existing = await revealThreadIfOpen(thread);
  if (existing) return existing;
  return replaceThreadInPane(thread, resolveOpenTargetPane(targetPane), 'preview');
}

export async function openThreadInNewPane(thread: Thread, insertIndex?: number): Promise<ThreadPane> {
  const existing = await revealThreadIfOpen(thread);
  if (existing) {
    commitPanePreview(existing.paneId);
    return existing;
  }
  const paneId = nextPaneId();
  const pane = createPane(paneId);
  addThreadPaneToLayout(paneId, insertIndex);
  return replaceThreadInPane(thread, pane, 'committed');
}

export function focusAdjacentPane(direction: -1 | 1): ThreadPane | null {
  const order = orderedPaneIds();
  const index = focusedPaneId ? order.indexOf(focusedPaneId) : -1;
  if (index < 0) return null;
  const nextId = order[index + direction];
  if (!nextId) return null;
  focusedPaneId = nextId;
  requestPaneReveal(nextId);
  return panes.get(nextId) ?? null;
}

export function moveFocusedPane(direction: -1 | 1): void {
  if (!focusedPaneId || !panes.has(focusedPaneId)) return;
  movePaneLayoutItem(focusedPaneId, direction);
  requestPaneReveal(focusedPaneId);
}

/**
 * Apply a Thread update across every UI surface that holds it: the
 * global threads registry (sidebar list) AND every pane currently
 * displaying it.
 *
 * Use anywhere a binding response or local mutation produces a fresh
 * Thread that should be visible everywhere — model change, agent-mode
 * toggle, plan-comments-sent, branch switch, env change, discussion
 * start, worktree remove, etc.
 *
 * Replaces the dual-write `pane.replaceThread(t); replaceThread(t);`
 * pattern that was scattered across ~13 call sites. Forgetting one
 * half of the pair caused desync between sidebar list and chat header.
 *
 * Server-event handlers in `events.ts` that need merge-aware semantics
 * (preserving local read markers / latest-completion timestamps across
 * server-pushed updates) keep using `syncThreadRow` — that helper does
 * `syncThread`'s fan-out plus the merge.
 */
export function syncThread(thread: Thread): void {
  replaceThreadInRegistry(thread);
  for (const pane of panes.values()) {
    if (pane.threadId !== thread.id || !pane.thread) continue;
    pane.replaceThread(thread);
  }
  touchProjectActivity(thread.projectId, thread.updatedAt);
}

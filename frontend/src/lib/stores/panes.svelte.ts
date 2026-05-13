import type { Thread } from '../types/models';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import { touchProjectActivity } from './projects.svelte';
import { replaceThread as replaceThreadInRegistry } from './threads.svelte';

// Active panes, keyed by pane ID. The current UI mounts only "main", but
// command routing and sidebar actions already go through this registry so
// adding visible panes does not require re-defining ownership later.
let panes: Map<string, ThreadPane> = $state(new Map());
let focusedPaneId: string = $state('main');
export type PaneActivation = 'preview' | 'committed';
let paneActivationById: Map<string, PaneActivation> = $state(new Map());

function registerPane(id: string, pane: ThreadPane, activation: PaneActivation = 'committed'): ThreadPane {
  panes = new Map(panes).set(id, pane);
  paneActivationById = new Map(paneActivationById).set(id, activation);
  return pane;
}

export function getMainPane(): ThreadPane {
  let main = panes.get('main');
  if (!main) {
    main = createThreadPane({ paneId: 'main' });
    registerPane('main', main, 'committed');
  }
  return main;
}

export function createPane(id: string): ThreadPane {
  const existing = panes.get(id);
  if (existing) return existing;
  const pane = createThreadPane({ paneId: id });
  return registerPane(id, pane, 'committed');
}

export function registerPaneForTest(id: string, pane: ThreadPane): void {
  registerPane(id, pane, 'committed');
}

export function getFocusedPane(): ThreadPane {
  return panes.get(focusedPaneId) ?? getMainPane();
}

export function focusPane(id: string): void {
  if (!panes.has(id)) return;
  focusedPaneId = id;
}

export function getFocusedPaneId(): string {
  return focusedPaneId;
}

export function getAllPanes(): Map<string, ThreadPane> {
  return panes;
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
  targetPane: string | ThreadPane = focusedPaneId,
  activation: PaneActivation = 'committed',
): Promise<ThreadPane> {
  const target = typeof targetPane === 'string'
    ? panes.get(targetPane) ?? getMainPane()
    : targetPane;
  if (!panes.has(target.paneId)) {
    registerPane(target.paneId, target, activation);
  } else {
    paneActivationById = new Map(paneActivationById).set(target.paneId, activation);
  }
  focusedPaneId = target.paneId;
  await target.switchThread(thread);
  return target;
}

export async function revealThreadIfOpen(thread: Thread): Promise<ThreadPane | null> {
  const pane = findPaneShowingThread(thread.id);
  if (!pane) return null;
  focusedPaneId = pane.paneId;
  return pane;
}

export async function openThreadInPane(
  thread: Thread,
  targetPane: string | ThreadPane = focusedPaneId,
): Promise<ThreadPane> {
  const existing = await revealThreadIfOpen(thread);
  if (existing) {
    commitPanePreview(existing.paneId);
    return existing;
  }
  return replaceThreadInPane(thread, targetPane, 'committed');
}

export async function openThreadFromNavigation(
  thread: Thread,
  targetPane: string | ThreadPane = focusedPaneId,
): Promise<ThreadPane> {
  const existing = await revealThreadIfOpen(thread);
  if (existing) return existing;
  return replaceThreadInPane(thread, targetPane, 'preview');
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

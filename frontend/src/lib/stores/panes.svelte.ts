import type { Thread } from '../types/models';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import { touchProjectActivity } from './projects.svelte';
import { replaceThread as replaceThreadInRegistry } from './threads.svelte';

// Active panes, keyed by pane ID. v1 has exactly one pane ("main").
let panes: Map<string, ThreadPane> = $state(new Map());

export function getMainPane(): ThreadPane {
  let main = panes.get('main');
  if (!main) {
    main = createThreadPane();
    panes = new Map(panes).set('main', main);
  }
  return main;
}

export function getAllPanes(): Map<string, ThreadPane> {
  return panes;
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

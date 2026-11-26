// The status mark of a thread's linked ticket (the sidebar row and the
// thread header) draws the group the thread's project puts the status in
// (`statusMarkGroup.ts#markGroup`), from that project's setup as
// `trackerSetup.svelte.ts` holds it. Until the setup is read the mark draws
// the tracker's group.
//
// The setup store is an Issues store, out of the startup graph: it loads
// through this module's dynamic import, once some listed thread or a
// shown thread header has a linked ticket, and never in a window with
// none. One hold per project with a linked thread (not per thread or row),
// kept by the sidebar's effect (`keepLinkedThreadSetups`): a project gains
// its hold when its first linked thread appears and loses it when its last
// one goes (unlinked, archived, deleted), so the setup reads, again on
// `tracker:setup_updated` and on each reconnect, are of those projects
// alone. A thread header holds its thread's project while it shows a
// link (`holdThreadLinkProject`), for a thread the sidebar does not list.

import { SvelteMap } from 'svelte/reactivity';
import { untrack } from 'svelte';
import type { Thread } from '../types/models';
import { getThreads } from './threads.svelte';
import { markGroup } from './statusMarkGroup';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';

type SetupStore = typeof import('./trackerSetup.svelte');

let setupStore = $state.raw<SetupStore | null>(null);
let loading: Promise<void> | null = null;
/** The projects a thread header holds, with how many headers hold each. */
const headerHolds = new SvelteMap<string, number>();
/** The setup store's hold of each project kept now. */
const held = new Map<string, () => void>();

/** The group `thread`'s linked ticket's mark draws; none without a link. */
export function linkMarkGroup(thread: Thread): string | undefined {
  const link = thread.ticketLink;
  if (!link) return undefined;
  const store = setupStore;
  const regroups = store && thread.projectId ? store.trackerRegroupsOf(thread.projectId, link.kind, link.site) : undefined;
  return markGroup(regroups, { id: link.statusId, group: link.statusGroup });
}

/** Holds `project`'s setup while a thread header shows its thread's link;
 * the returned function releases it. No project holds nothing. */
export function holdThreadLinkProject(project: string | null | undefined): () => void {
  if (!project) return () => {};
  headerHolds.set(project, (headerHolds.get(project) ?? 0) + 1);
  let released = false;
  return () => {
    if (released) return;
    released = true;
    const n = (headerHolds.get(project) ?? 1) - 1;
    if (n > 0) headerHolds.set(project, n);
    else headerHolds.delete(project);
  };
}

/** Keeps one setup hold for each project with a linked thread, listed or
 * shown in a header, loading the setup store for the first. Run from an
 * effect: it reads the threads, the header holds and the store's load. */
export function keepLinkedThreadSetups(): void {
  const wanted = new Set<string>();
  for (const thread of getThreads()) {
    if (thread.ticketLink && thread.projectId) wanted.add(thread.projectId);
  }
  for (const project of headerHolds.keys()) wanted.add(project);
  const store = setupStore;
  untrack(() => {
    for (const [project, release] of held) {
      if (wanted.has(project)) continue;
      release();
      held.delete(project);
    }
    if (wanted.size === 0) return;
    if (!store) {
      loading ??= loadSetupStore();
      return;
    }
    for (const project of wanted) {
      if (!held.has(project)) held.set(project, store.acquireTrackerSetup(project));
    }
  });
}

async function loadSetupStore(): Promise<void> {
  try {
    setupStore = await import('./trackerSetup.svelte');
  } catch (err) {
    // The marks draw the tracker's groups; the next change of the linked
    // threads tries again.
    loading = null;
    reportFrontendDiagnostic("A linked ticket's status mark could not load its project's status groups", err instanceof Error ? err.message : String(err));
  }
}

/** Releases every hold (the sidebar's teardown). */
export function releaseLinkedThreadSetups(): void {
  for (const release of held.values()) release();
  held.clear();
}

/** Whether the setup store was asked for: its load started. */
export function linkedThreadSetupsLoadStartedForTest(): boolean {
  return loading !== null;
}

export function resetThreadLinkMarksForTest(): void {
  releaseLinkedThreadSetups();
  headerHolds.clear();
  setupStore = null;
  loading = null;
}

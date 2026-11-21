// The sidebar's thread list is two catalogs: threads, and the groups that
// gather them. Read apart, a computer's rows would show in one arrangement
// when the first answer commits and move when the second does. Every read
// of both (boot, hello, gap resync, a history generation change) is one
// round from here.
//
// A computer the sidebar does not list yet (catalogTreeListed) commits each
// answer as it arrives; the sidebar holds it until neither read is in
// flight (./catalogLoad.svelte.ts). A listed computer's two answers commit
// together, in one synchronous block, through the round:
//
//   - When both answer on time, they commit together when the round's
//     on-time window ends.
//   - A thread answer never waits past that window: the boot's layout
//     restore validates saved panes against the thread rows, and a pane
//     whose thread is missing is dropped. So when the group read passes the
//     deadline, the threads commit at the window's end and the late group
//     answer commits alone, which can move rows. That is the known limit.
//   - A group answer waits for a thread answer still out, on time or late,
//     and commits with it; two late answers commit together.
//
// A computer's slot is released when its read can no longer deliver: the
// read ended, or a newer read of that catalog began (beginCatalogRead's
// release, ./computerCatalogReads.ts). A released slot never holds its
// partner.
import { untrack } from 'svelte';
import { attachedBackends } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { onBackendIdentity } from '../transport/backendIdentity';
import { catalogTreeListed } from './catalogLoad.svelte';
import type { CatalogRoundRead } from './computerRows';
import { readProjectCatalog } from './projects.svelte';
import { loadThreadGroups } from './threadGroups.svelte';
import { getThreads, loadThreads } from './threads.svelte';
import type { Thread } from '../types/models';

type PairedKind = 'threads' | 'groups';

interface Slot {
  state: 'out' | 'delivered' | 'done';
  commit: (() => void) | null;
  onTime: boolean;
}

class ThreadCatalogRound {
  private readonly slots = new Map<BackendKey, Record<PairedKind, Slot>>();
  private openWindows = 2;

  constructor(paired: readonly BackendKey[]) {
    for (const backend of paired) {
      this.slots.set(backend, {
        threads: { state: 'out', commit: null, onTime: false },
        groups: { state: 'out', commit: null, onTime: false },
      });
    }
  }

  get pairsAny(): boolean {
    return this.slots.size > 0;
  }

  read(kind: PairedKind): CatalogRoundRead {
    return {
      pairs: (backend) => this.slots.has(backend),
      deliver: (backend, commit, onTime) => {
        const slot = this.slots.get(backend)?.[kind];
        if (!slot || slot.state !== 'out') return;
        Object.assign(slot, { state: 'delivered', commit, onTime });
        this.flush(backend);
      },
      release: (backend) => {
        const slot = this.slots.get(backend)?.[kind];
        if (!slot || slot.state === 'done') return;
        // A delivered commit runs anyway: its read is stale now, so it only
        // ends its journal (commitAnswer checks the read is still current).
        const commit = slot.state === 'delivered' ? slot.commit : null;
        Object.assign(slot, { state: 'done', commit: null });
        try { commit?.(); } finally { this.flush(backend); }
      },
      windowEnded: () => {
        if (--this.openWindows > 0) return;
        for (const backend of this.slots.keys()) this.flush(backend);
      },
    };
  }

  // Run every commit of `backend` that may run now, threads first, in one
  // synchronous block.
  private flush(backend: BackendKey): void {
    const slots = this.slots.get(backend);
    if (!slots) return;
    const { threads, groups } = slots;
    const windowEnded = this.openWindows === 0;
    const run: Array<() => void> = [];
    if (threads.state === 'delivered' && (groups.state !== 'out' || (threads.onTime && windowEnded))) run.push(take(threads));
    if (groups.state === 'delivered' && threads.state !== 'out') run.push(take(groups));
    if (threads.state === 'done' && groups.state === 'done') this.slots.delete(backend);
    runAll(run);
  }
}

function take(slot: Slot): () => void {
  const commit = slot.commit!;
  Object.assign(slot, { state: 'done', commit: null });
  return commit;
}

function runAll(commits: Array<() => void>): void {
  const [first, ...rest] = commits;
  if (!first) return;
  try { first(); } finally { runAll(rest); }
}

/**
 * Read the computers' threads and groups as one round, every computer's or
 * `only`'s, and resolve with the thread rows once the thread read has
 * committed. With a listed computer in the round, it resolves after the
 * round's on-time window, when that computer's paired commit has run, so a
 * caller validating saved panes sees every thread answer that arrived on
 * time. Group failures are settled into the catalog state, which shows
 * them and retries.
 */
export async function loadThreadCatalogs(only?: BackendKey): Promise<readonly Thread[]> {
  const paired = untrack(() => attachedBackends()
    .filter((entry) => (only === undefined || entry.id === only) && catalogTreeListed(entry.id))
    .map((entry) => entry.id));
  const round = new ThreadCatalogRound(paired);
  const threads = loadThreads({ only, round: round.read('threads') });
  const groups = loadThreadGroups({ only, round: round.read('groups') });
  void groups.catch(() => {
    // Each computer's failure is in the catalog state already.
  });
  if (!round.pairsAny) return await threads;
  await Promise.allSettled([threads, groups]);
  await threads;
  // The paired commits ran when the window ended, after the thread read
  // resolved: answer with the rows as they are now.
  return getThreads();
}

// A history generation observed mid-session (a restored database) has no
// hello behind it: re-read every catalog of that computer, whatever its
// phase, so its rows describe the history it has now.
onBackendIdentity((_identity, backend, change) => {
  if (change !== 'generation') return;
  void loadThreadCatalogs(backend).catch(() => {
    // Each catalog's failure is in the catalog state already.
  });
  void readProjectCatalog(backend);
});

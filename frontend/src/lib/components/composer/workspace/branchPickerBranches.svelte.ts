// The branch picker's branch list, read while the picker is open. Opening
// reads it and starts a background remote fetch that rereads it (and calls
// `onFetched`) when the fetch brought anything. While open, the list
// follows the pane's thread, workspace and checked-out branch. A reply for
// a thread or read that is no longer current is dropped. Create during
// component initialization: it owns an effect.

import { GitListBranches, GitMaybeFetchRemotes } from '../../../stores/bindings';
import type { ThreadPane } from '../../../stores/thread.svelte';
import type { GitBranch, WorkspaceRef } from '../../../types/git';
import { userFacingError } from '../../../utils/userFacingError';
import { workspaceKeyForRef } from '../../../utils/workspaceKey';

export interface PickerBranches {
  readonly branches: readonly GitBranch[];
  readonly loading: boolean;
  /** Why the open list is empty; cleared by the next successful list. */
  readonly error: string | null;
  /** Reads the list for the picker that just opened. */
  open(): Promise<void>;
  /** A list another call answered with, for the thread it ran for. */
  replace(threadId: string, branches: GitBranch[]): void;
}

export function createPickerBranches(opts: {
  pane: () => Pick<ThreadPane, 'thread' | 'workspace'>;
  /** The branch the pane's checkout is on, '' when detached. */
  checkoutBranch: () => string;
  isOpen: () => boolean;
  onFetched: (ws: WorkspaceRef) => void;
}): PickerBranches {
  let branches = $state.raw<GitBranch[]>([]);
  let loading = $state(false);
  let error = $state<string | null>(null);
  let lastKey = '';
  let seq = 0;

  function keyOf(threadId: string | undefined, ws: WorkspaceRef | null, branch: string): string {
    return `${threadId ?? ''}\0${workspaceKeyForRef(ws) ?? ''}\0${branch}`;
  }

  async function read(threadId: string, ws: WorkspaceRef): Promise<void> {
    const at = ++seq;
    try {
      const res = (await GitListBranches(ws)) as GitBranch[] | null;
      if (at !== seq || opts.pane().thread?.id !== threadId || !opts.isOpen()) return;
      branches = Array.isArray(res) ? res : [];
      error = null;
    } catch (err) {
      if (at !== seq || opts.pane().thread?.id !== threadId || !opts.isOpen()) return;
      branches = [];
      error = userFacingError(err);
    }
  }

  $effect(() => {
    const pane = opts.pane();
    const threadId = pane.thread?.id;
    const ws = pane.workspace;
    const key = keyOf(threadId, ws, opts.checkoutBranch());
    if (!opts.isOpen() || !threadId || !ws) {
      lastKey = key;
      return;
    }
    if (key === lastKey) return;
    lastKey = key;
    void read(threadId, ws);
  });

  return {
    get branches() {
      return branches;
    },
    get loading() {
      return loading;
    },
    get error() {
      return error;
    },
    async open() {
      const { thread, workspace: ws } = opts.pane();
      if (!thread || !ws || loading) return;
      lastKey = keyOf(thread.id, ws, opts.checkoutBranch());
      loading = true;
      error = null;
      const first = read(thread.id, ws);
      void (async () => {
        let fetched: boolean;
        try {
          fetched = !!(await GitMaybeFetchRemotes(ws));
        } catch (err) {
          console.error('background fetch failed:', err);
          return;
        }
        // Reread what the picker lists now: the pane may have moved to
        // another workspace while the fetch ran.
        const now = opts.pane();
        if (!fetched || !opts.isOpen() || !now.thread || !now.workspace) return;
        opts.onFetched(now.workspace);
        await read(now.thread.id, now.workspace);
      })();
      try {
        await first;
      } finally {
        loading = false;
      }
    },
    replace(threadId, next) {
      if (opts.pane().thread?.id === threadId) branches = next;
    },
  };
}

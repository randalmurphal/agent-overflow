// The thread title's regenerate action, shared by its two surfaces: the
// refresh glyph beside the title (ThreadTitleRegenerateButton, desktop)
// and the row in the compact header's actions menu (ChatHeaderActions).
//
// Pending state is thread-entity state in threadTitleGeneration (the run
// outlives any component and its completion arrives as an event), so a
// remount mid-run resumes spinning. RegenerateThreadTitle runs provider
// CLIs on the host, so it rides `threads:operate`: a remote view-only
// session gets a disabled control rather than an RPC the transport would
// refuse. Must be called from component init context.

import { threadHasScope } from '../../transport/entityScopes';
import type { PaneSession } from '../../stores/threadPaneRoles';
import {
  regenerateThreadTitle,
  titleGenerationPending,
} from '../../stores/threadTitleGeneration.svelte';

export interface ThreadTitleRegenerate {
  readonly pending: boolean;
  readonly ungranted: boolean;
  /** Tooltip: the action, or why it is unavailable. */
  readonly title: string;
  run(): void;
}

export function createThreadTitleRegenerate(getPane: () => PaneSession): ThreadTitleRegenerate {
  const pending = $derived.by(() => {
    const threadId = getPane().threadId;
    return threadId ? titleGenerationPending(threadId) : false;
  });
  const ungranted = $derived.by(() => {
    const pane = getPane();
    return !threadHasScope('threads:operate', pane.threadId, pane.thread?.projectId);
  });
  const title = $derived(ungranted ? 'Not granted to this device' : 'Regenerate title');
  return {
    get pending() { return pending; },
    get ungranted() { return ungranted; },
    get title() { return title; },
    run() {
      const threadId = getPane().threadId;
      if (threadId) void regenerateThreadTitle(threadId);
    },
  };
}

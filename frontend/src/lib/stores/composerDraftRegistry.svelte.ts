import type { ComposerDraftStore } from './composerDraft.svelte';

const draftsByPane = new Map<string, ComposerDraftStore>();

/**
 * Register a pane's composer-draft store. First registration wins —
 * subsequent calls for the same pane are recorded as passengers and
 * return a no-op dispose. This handles the case where ChatView (the
 * normal parent) AND Composer (a defensive registrant for standalone
 * test mounts and a future design-only composer path) both try to
 * register the same store reference. A reference-equal dispose check
 * cannot distinguish the two registrants, so without first-wins we
 * would tear down the entry as soon as either component unmounted —
 * leaving subsequent `getComposerDraftForPane` reads stale even
 * though the owning pane was still alive.
 */
export function registerComposerDraft(
  paneId: string,
  draft: ComposerDraftStore,
): () => void {
  if (draftsByPane.has(paneId)) {
    return () => {};
  }
  draftsByPane.set(paneId, draft);
  return () => {
    if (draftsByPane.get(paneId) === draft) {
      draftsByPane.delete(paneId);
    }
  };
}

/**
 * Look up a pane's composer draft store. Returns undefined if the pane
 * doesn't currently have one registered (pane closed, never mounted,
 * or design-mode without a composer). Used by the keybinding-driven
 * Stop flow (`thread.interrupt`) to read draft state for the revert
 * predicate without piping the store through every command's context.
 */
export function getComposerDraftForPane(paneId: string): ComposerDraftStore | undefined {
  return draftsByPane.get(paneId);
}

export function resetComposerDraftRegistryForTest(): void {
  draftsByPane.clear();
}

import type { ComposerDraftStore } from './composerDraft.svelte';

const draftsByPane = new Map<string, ComposerDraftStore>();

export function registerComposerDraft(
  paneId: string,
  draft: ComposerDraftStore,
): () => void {
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

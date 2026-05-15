import type { TerminalChip } from '../types/draft';
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

export function addTerminalChipToPaneDraft(paneId: string, chip: TerminalChip): boolean {
  const draft = draftsByPane.get(paneId);
  if (!draft) return false;
  draft.addTerminalChip(chip);
  return true;
}

export function resetComposerDraftRegistryForTest(): void {
  draftsByPane.clear();
}

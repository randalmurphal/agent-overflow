import type { ComposerDraftSnapshot } from '../stores/composerDraftSnapshots';
import { remapImagePlaceholders } from './imagePlaceholders';

type DraftFragment = Pick<ComposerDraftSnapshot, 'content' | 'attachments'>
  & Partial<Pick<ComposerDraftSnapshot, 'terminalChips' | 'sourceProposedPlan'>>;

/** Recovery prepends earlier text to later edits, preserving their attachment identities. */
export function prependDraftSnapshot(first: DraftFragment, current: ComposerDraftSnapshot): ComposerDraftSnapshot {
  const attachments = uniqueById(first.attachments, current.attachments);
  const before = remapImagePlaceholders(first.content, first.attachments, attachments);
  const after = remapImagePlaceholders(current.content, current.attachments, attachments);
  return {
    content: after.trim() === '' ? before : before.trim() === '' ? after : `${before}\n\n${after}`,
    attachments,
    terminalChips: uniqueById(first.terminalChips ?? [], current.terminalChips),
    sourceProposedPlan: current.sourceProposedPlan ?? first.sourceProposedPlan ?? null,
  };
}

function uniqueById<T extends { id: string }>(first: T[], second: T[]): T[] {
  const ids = new Set<string>();
  return [...first, ...second].filter((entry) => {
    if (ids.has(entry.id)) return false;
    ids.add(entry.id);
    return true;
  });
}

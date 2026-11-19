import type { ComposerDraftSnapshot } from '../stores/composerDraftSnapshots';
import type { Item } from '../types/models';
import { ensureImagePlaceholders } from './imagePlaceholders';
import {
  parseUserMessageMeta,
  restoredDraftSnapshotAttachmentsFromUserItem,
  sourceProposedPlanFromUserMessageMeta,
} from './userMessageMeta';

/** content replaces the row's text, as when only part of it is the person's. */
export function restoredDraftSnapshotFromUserItem(item: Item, content = item.summary): ComposerDraftSnapshot {
  const meta = parseUserMessageMeta(item.meta);
  const attachments = restoredDraftSnapshotAttachmentsFromUserItem(item);
  return {
    content: ensureImagePlaceholders(content, attachments),
    attachments,
    terminalChips: [],
    sourceProposedPlan: sourceProposedPlanFromUserMessageMeta(meta.sourceProposedPlan),
  };
}

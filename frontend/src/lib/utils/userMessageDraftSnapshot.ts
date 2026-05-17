import type { ComposerDraftSnapshot } from '../stores/composerDraftSnapshots';
import type { Item } from '../types/models';
import { ensureImagePlaceholders } from './imagePlaceholders';
import {
  parseUserMessageMeta,
  restoredDraftSnapshotAttachmentsFromUserItem,
  sourceProposedPlanFromUserMessageMeta,
} from './userMessageMeta';

export function restoredDraftSnapshotFromUserItem(item: Item): ComposerDraftSnapshot {
  const meta = parseUserMessageMeta(item.meta);
  const attachments = restoredDraftSnapshotAttachmentsFromUserItem(item);
  return {
    content: ensureImagePlaceholders(item.summary, attachments),
    attachments,
    terminalChips: [],
    sourceProposedPlan: sourceProposedPlanFromUserMessageMeta(meta.sourceProposedPlan),
  };
}

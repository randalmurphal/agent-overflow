import { resolveThreadStatusPill, type ThreadStatusPill } from '../../utils/threadStatusPill';
import { getEffectiveThreadStatus, type ThreadLiveStatus } from '../../stores/threadStatuses.svelte';
import type { Thread } from '../../types/models';

export const PANE_ATTENTION_DOT_OFFSET = 12;

export interface PaneAttentionDotModel {
  status: ThreadLiveStatus;
  pill: ThreadStatusPill;
}

export function resolvePaneAttentionDot(thread: Thread | null): PaneAttentionDotModel | null {
  if (!thread) return null;
  const status = getEffectiveThreadStatus(thread);
  const pill = resolveThreadStatusPill(thread, status);
  if (!pill) return null;
  return { status, pill };
}

export function paneDotAnchorX(paneLeft: number): number {
  return paneLeft + PANE_ATTENTION_DOT_OFFSET;
}

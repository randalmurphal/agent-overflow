import type { Thread } from '../../types/models';
import { hasUnread } from '../../utils/threadStatusPill';
import {
  getEffectiveThreadStatus,
  type ThreadLiveStatus,
} from '../../stores/threadStatuses.svelte';
import { getFocusedPaneId } from '../../stores/panes.svelte';
import { getPaneLayoutItems } from '../../stores/paneLayout.svelte';
import { isCompactLayout } from '../../stores/layoutMode.svelte';

// The pane header's one status surface: the hairline laid over the header
// separator (PaneHeaderLine.svelte). It answers "which pane has my keys" and
// "which other pane wants me", never both at once:
//
//  - Nothing when there is nothing to tell apart: compact layout shows one
//    pane at a time, and a strip holding a single pane has one place input
//    can go and everything that pane wants is already in front of the user.
//  - The focused pane is accent, whatever its thread is doing. A focused
//    pane's Completed / Failed / Interrupted states clear on focus (the
//    read-mark effect in ChatView), and the rest (approval, input, plan,
//    setup) already have a panel or banner in the pane itself.
//  - An unfocused pane shows its thread's attention color from the sidebar
//    status table, minus running: a background turn is not something the
//    user has to act on, and the pane's own activity rail shows it.
//
// Focus is the RAW focused pane id (not getFocusedThreadPaneId): when a
// companion holds focus its own header is accent, and the source thread's
// header must not light up too.

export type PaneHeaderLineColor = 'accent' | 'success' | 'warning' | 'info' | 'error';

export interface PaneHeaderLine {
  color: PaneHeaderLineColor;
  /** Accessible name for the state. Never rendered visibly. */
  label: string;
}

export interface PaneHeaderLineInput {
  focused: boolean;
  paneCount: number;
  compact: boolean;
  thread: Pick<Thread, 'lastReadAt' | 'latestTurnCompletedAt'> | null;
  status: ThreadLiveStatus | null;
}

export function resolvePaneHeaderLine(input: PaneHeaderLineInput): PaneHeaderLine | null {
  if (input.compact) return null;
  if (input.paneCount < 2) return null;
  if (input.focused) return { color: 'accent', label: 'Focused pane' };
  if (!input.thread || input.status === null) return null;
  switch (input.status) {
    case 'error':
      return { color: 'error', label: 'Failed' };
    case 'pending-approval':
      return { color: 'warning', label: 'Pending Approval' };
    case 'awaiting-input':
      return { color: 'info', label: 'Awaiting Input' };
    case 'running':
      return null;
    case 'setup-failed':
      return { color: 'warning', label: 'Setup Failed' };
    case 'plan-ready':
      // The sidebar dot uses accent for a settled plan. On the header line
      // accent means focus, so a waiting plan reads as a finished turn.
      return { color: 'success', label: 'Plan Ready' };
    case 'interrupted':
      return { color: 'warning', label: 'Interrupted' };
    case 'idle':
      if (hasUnread(input.thread)) return { color: 'success', label: 'Completed' };
      return null;
  }
}

export function paneHeaderLineFor(paneId: string, thread: Thread | null): PaneHeaderLine | null {
  return resolvePaneHeaderLine({
    focused: getFocusedPaneId() === paneId,
    paneCount: getPaneLayoutItems().length,
    compact: isCompactLayout(),
    thread,
    status: thread ? getEffectiveThreadStatus(thread) : null,
  });
}

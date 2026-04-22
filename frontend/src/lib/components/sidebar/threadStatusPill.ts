// Pure classification for the thread-row status pill and unread signal.
//
// Replaces the older threadRowStatus.ts which only emitted a dot class.
// This version produces a richer {label, dotClass, labelClass, pulse}
// record so the sidebar can show the mode-aware forge-style pill
// ("Planning", "Designing", "Working", "Discussing", "Pending approval",
// "Error", "Completed") next to the title.
//
// Keep this file free of Svelte imports so its behaviour stays
// table-drivable from unit tests.

import type { ThreadLiveStatus } from '../../stores/threadStatuses.svelte';
import type { Thread } from '../../types/models';

export interface ThreadStatusPill {
  /** Visible text next to the title. `null` → no pill, only the dot. */
  label: string;
  /** Tailwind classes applied to the leading dot. */
  dotClass: string;
  /** Tailwind classes applied to the label text. */
  labelClass: string;
  /** True when the dot should animate (running / awaiting input). */
  pulse: boolean;
}

/**
 * hasUnread returns true when the thread has activity the user hasn't
 * seen yet. Null `lastReadAt` counts as read so pre-migration rows
 * don't all light up on first deploy — the auto-mark-read effect
 * populates the column the first time the user switches in.
 */
export function hasUnread(thread: Pick<Thread, 'lastReadAt' | 'updatedAt'>): boolean {
  if (thread.lastReadAt == null) return false;
  return thread.updatedAt > thread.lastReadAt;
}

/**
 * resolveThreadStatusPill picks the right pill for a row. Returns
 * `null` when the row should show nothing more than its title + time
 * (the common idle case). Resolution order:
 *   1. error  → "Error"
 *   2. pending-approval → "Pending approval" (user must act)
 *   3. running → mode-aware (Planning / Designing / Discussing / Working)
 *   4. idle + unread → "Completed"
 *   5. idle + read   → null (no pill)
 *
 * The full behaviour matches forge's `Sidebar.logic.ts`; we picked a
 * subset of the status kinds that map cleanly onto agent-overflow's
 * existing ThreadLiveStatus projection. If we later add richer states
 * (connecting, paused, plan-ready) extend this table.
 */
export function resolveThreadStatusPill(
  thread: Pick<Thread, 'mode' | 'lastReadAt' | 'updatedAt'>,
  liveStatus: ThreadLiveStatus,
): ThreadStatusPill | null {
  if (liveStatus === 'error') {
    return {
      label: 'Error',
      dotClass: 'bg-error',
      labelClass: 'text-error',
      pulse: false,
    };
  }
  if (liveStatus === 'pending-approval') {
    return {
      label: 'Pending approval',
      dotClass: 'bg-accent',
      labelClass: 'text-accent',
      pulse: true,
    };
  }
  if (liveStatus === 'running') {
    const running = {
      dotClass: 'bg-warning',
      labelClass: 'text-warning',
      pulse: true,
    };
    switch (thread.mode) {
      case 'plan':
        return { label: 'Planning', ...running };
      case 'design':
        return { label: 'Designing', ...running };
      case 'discussion':
        return { label: 'Discussing', ...running };
      default:
        return { label: 'Working', ...running };
    }
  }
  // idle
  if (hasUnread(thread)) {
    return {
      label: 'Completed',
      dotClass: 'bg-success',
      labelClass: 'text-success',
      pulse: false,
    };
  }
  return null;
}

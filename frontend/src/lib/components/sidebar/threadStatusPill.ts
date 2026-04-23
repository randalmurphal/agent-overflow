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
  /**
   * Optional utility class applied to the row's outer container. Used
   * for the pulsing glow-ring around pending-approval / awaiting-input
   * rows so the sidebar can catch the user's attention when the
   * provider is blocked on them. `null` / undefined → no glow.
   * Defined in app.css (`.status-glow-warning`, `.status-glow-accent`).
   */
  glowClass?: string;
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
 *   1. error            → "Error"
 *   2. pending-approval → "Pending approval" (blocking tool permission)
 *   3. awaiting-input   → "Awaiting input" (agent asking a question)
 *   4. running          → mode-aware (Planning / Designing / Discussing / Working)
 *   5. plan-ready       → "Plan ready" (settled plan awaiting accept/edit/reject)
 *   6. idle + unread    → "Completed"
 *   7. idle + read      → null (no pill)
 *
 * Priority matches forge's `Sidebar.logic.ts`. Colors — pending-approval
 * shares the running amber because both are "agent pausing on an action
 * the user needs to resolve"; awaiting-input and plan-ready use accent
 * violet to read as calmer user-prompts. Plan-ready doesn't pulse — the
 * plan isn't actively working; awaiting-input does pulse because the
 * agent is actively stuck.
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
      dotClass: 'bg-warning',
      labelClass: 'text-warning',
      pulse: true,
      glowClass: 'status-glow-warning',
    };
  }
  if (liveStatus === 'awaiting-input') {
    return {
      label: 'Awaiting input',
      dotClass: 'bg-accent',
      labelClass: 'text-accent',
      pulse: true,
      glowClass: 'status-glow-accent',
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
  if (liveStatus === 'plan-ready') {
    return {
      label: 'Plan ready',
      dotClass: 'bg-accent',
      labelClass: 'text-accent',
      pulse: false,
    };
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

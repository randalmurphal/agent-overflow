// Pure classification for the thread-row status pill and unread signal.
//
// Replaces the older threadRowStatus.ts which only emitted a dot class.
// This version produces a richer {label, dotClass, labelClass, pulse}
// record so the sidebar can show the mode-aware forge-style pill
// ("Planning", "Designing", "Working", "Discussing", "Pending approval",
// "Interrupted", "Failed", "Completed") next to the title.
//
// Keep this file free of Svelte imports so its behaviour stays
// table-drivable from unit tests.

import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { Thread } from '../types/models';

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
   * Defined in app.css (`.status-glow-warning`, `.status-glow-info`).
   */
  glowClass?: string;
}

export interface EffectiveThreadStatusOptions {
  /**
   * Suppress the durable interrupted fallback while the backend live-state
   * snapshot is in flight. Without this, a refresh can briefly label an
   * actually-running turn as Interrupted from stale row metadata.
   */
  suppressDurableInterrupted?: boolean;
}

const RUNNING_SUCCESS = {
  dotClass: 'bg-success',
  labelClass: 'text-success',
  pulse: true,
} as const;

/**
 * hasUnread returns true when the thread has activity the user hasn't
 * seen yet. The activity clock is the latest completed turn, not broad
 * thread updatedAt; metadata-only changes should not show "Completed".
 * Null `lastReadAt` counts as read so pre-migration rows don't all light up
 * on first deploy.
 */
export function hasUnread(thread: Pick<Thread, 'lastReadAt' | 'latestTurnCompletedAt'>): boolean {
  if (thread.latestTurnCompletedAt == null) return false;
  if (thread.lastReadAt == null) return false;
  return thread.latestTurnCompletedAt > thread.lastReadAt;
}

/**
 * Live events win first. When no live event is present, durable Thread-row
 * projections restore boot-time status for a failed worktree setup, prior
 * interrupted turns, and actionable plans.
 *
 * The early return on a non-idle live status is what makes "setup-failed must
 * never mask error / pending-approval / awaiting-input" structural rather than
 * a matter of ordering below: a thread whose provider is blocked on the user
 * reports that, whatever its worktree's provisioning state is.
 *
 * Among the durable fallbacks, setup-failed goes first. It is the only one
 * naming a concrete failure with a repair the user has to run — Interrupted is
 * cleared by sending the next message, and Plan Ready is informational.
 */
export function resolveEffectiveThreadStatus(
  thread: Pick<Thread, 'hasIncompleteTurn' | 'hasActionableProposedPlan' | 'worktreeSetupState'>,
  liveStatus: ThreadLiveStatus,
  options: EffectiveThreadStatusOptions = {},
): ThreadLiveStatus {
  if (liveStatus !== 'idle') return liveStatus;
  if (thread.worktreeSetupState === 'failed') return 'setup-failed';
  if (thread.hasIncompleteTurn && !options.suppressDurableInterrupted) return 'interrupted';
  if (thread.hasActionableProposedPlan) return 'plan-ready';
  return 'idle';
}

/**
 * resolveThreadStatusPill picks the right pill for a row. Returns
 * `null` when the row should show nothing more than its title + time
 * (the common idle case). Resolution order:
 *   1. error            → "Failed"
 *   2. pending-approval → "Pending approval" (blocking tool permission)
 *   3. awaiting-input   → "Awaiting input" (agent asking a question)
 *   4. running          → mode-aware (Planning / Designing / Discussing / Working)
 *   5. setup-failed     → "Setup Failed" (worktree setup recipe did not finish)
 *   6. plan-ready       → "Plan ready" (settled plan awaiting accept/edit/reject)
 *   7. interrupted      → "Interrupted" (prior app closed mid-turn)
 *   8. idle + unread    → "Completed"
 *   9. idle + read      → null (no pill)
 *
 * Priority and colors match forge's `Sidebar.logic.ts`: running / completed
 * states use success green, discussion uses the running-blue outline,
 * awaiting-input uses info blue, and plan-ready uses the app primary accent.
 */
export function resolveThreadStatusPill(
  thread: Pick<Thread, 'mode' | 'lastReadAt' | 'latestTurnCompletedAt'>,
  liveStatus: ThreadLiveStatus,
): ThreadStatusPill | null {
  if (liveStatus === 'error') {
    return {
      label: 'Failed',
      dotClass: 'bg-error',
      labelClass: 'text-error',
      pulse: false,
    };
  }
  if (liveStatus === 'pending-approval') {
    return {
      label: 'Pending Approval',
      dotClass: 'bg-warning',
      labelClass: 'text-warning',
      pulse: true,
      glowClass: 'status-glow-warning',
    };
  }
  if (liveStatus === 'awaiting-input') {
    return {
      label: 'Awaiting Input',
      dotClass: 'bg-info',
      labelClass: 'text-info',
      pulse: true,
      glowClass: 'status-glow-info',
    };
  }
  if (liveStatus === 'running') {
    switch (thread.mode) {
      case 'plan':
        return { label: 'Planning', ...RUNNING_SUCCESS };
      case 'design':
        return { label: 'Designing', ...RUNNING_SUCCESS };
      case 'discussion':
        return {
          label: 'Discussing',
          dotClass: 'border border-info bg-transparent',
          labelClass: 'text-info',
          pulse: false,
        };
      default:
        return { label: 'Working', ...RUNNING_SUCCESS };
    }
  }
  if (liveStatus === 'setup-failed') {
    // Warning, not error: the thread and its worktree are usable — the agent
    // can even repair the setup itself. Red is reserved for a turn that
    // actually failed. No glow: nothing is blocked waiting on the user.
    return {
      label: 'Setup Failed',
      dotClass: 'bg-warning',
      labelClass: 'text-warning',
      pulse: false,
    };
  }
  if (liveStatus === 'plan-ready') {
    return {
      label: 'Plan Ready',
      dotClass: 'bg-accent',
      labelClass: 'text-accent',
      pulse: false,
    };
  }
  if (liveStatus === 'interrupted') {
    return {
      label: 'Interrupted',
      dotClass: 'bg-warning',
      labelClass: 'text-warning',
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

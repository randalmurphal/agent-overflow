// The needs-attention sweep (UI-SPEC §4.4). One glue module over the two
// workflow stores so the keyboard commands, the row click, and the
// act-then-auto-advance path all step the same cursor the same way.
//
// The set is every parked ROOT run — every `needs-human` reason plus `failed`
// plus done-awaiting-disposition — app-wide, respecting the project filter,
// oldest-parked first, wrapping. A child run's park appears as its root (one
// sweep stop per tree), which falls out of the root-only filter: the child's
// resting state is what parked the root.

import type { WorkItem } from '../types/workflow';
import { getWorkflowReceipts, getWorkflowRuns } from './workflowRuns.svelte';
import { stepWorkflowSweep, workflowSweepItems, workflowSweepPosition } from './workflowData';
import {
  getWorkflowProjectFilter,
  getWorkflowSweepIndex,
  pushWorkflowAllClear,
  pushWorkflowRunDetail,
  setWorkflowSweepCursor,
} from './workflowsOverlay.svelte';

export function workflowSweepSet(): WorkItem[] {
  return workflowSweepItems(getWorkflowRuns(), getWorkflowReceipts(), getWorkflowProjectFilter());
}

/** Runs already resolved this session — visited, receipt shown, never again. */
function resolvedThisSession(): ReadonlySet<string> {
  return new Set(getWorkflowReceipts().keys());
}

/** 1-based position + total for the "3 of 7" counter on a parked run (§4.1). */
export function workflowSweepCounter(itemId: string): { position: number; total: number } | null {
  const items = workflowSweepSet();
  if (items.length === 0) return null;
  const index = workflowSweepPosition(items, itemId, getWorkflowSweepIndex());
  if (index < 0) return null;
  return { position: index + 1, total: items.length };
}

/** Open a run at its sweep position — the needs-attention row click (§3.2). */
export function enterWorkflowSweep(itemId: string): void {
  const items = workflowSweepSet();
  const index = items.findIndex((item) => item.id === itemId);
  pushWorkflowRunDetail(itemId, { sweep: index >= 0, sweepIndex: index });
}

/**
 * Step to the next / previous run that still needs attention. Exhaustion
 * pushes all-clear; the caller never has to check.
 */
export function advanceWorkflowSweep(direction: -1 | 1, fromItemId = ''): void {
  const items = workflowSweepSet();
  const anchor = fromItemId
    ? workflowSweepPosition(items, fromItemId, getWorkflowSweepIndex())
    : getWorkflowSweepIndex();
  const next = stepWorkflowSweep(items, anchor, direction, resolvedThisSession());
  if (!next) {
    pushWorkflowAllClear();
    return;
  }
  setWorkflowSweepCursor(true, next.index);
  pushWorkflowRunDetail(next.itemId, { sweep: true, sweepIndex: next.index });
}

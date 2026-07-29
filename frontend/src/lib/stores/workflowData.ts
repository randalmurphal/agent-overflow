// Pure projections over the workflow run record. Everything the overlay
// renders that is not a Svelte concern lives here so it stays table-testable:
// the reactive cache (workflowRuns.svelte.ts) applies these, the components
// only render their output.
//
// Vocabulary matches the spec: workflow = definition, run = `work_items` row,
// phase = a step of a run, unit = one fan-out member, child run = a call
// phase's invoked run.

import type {
  WorkItem,
  WorkflowDefinitionListing,
  WorkflowItemStateEvent,
  WorkflowResolvedReceipt,
} from '../types/workflow';
import { parseWorkflowDisposition } from '../types/workflow';

export function workflowDefinitionMeta(definition: WorkflowDefinitionListing): string {
  const phases = `${definition.phaseCount} ${definition.phaseCount === 1 ? 'phase' : 'phases'}`;
  const humanGateCount = definition.humanGateCount ?? 0;
  if (humanGateCount <= 0) return phases;
  const gates = `${humanGateCount} human ${humanGateCount === 1 ? 'gate' : 'gates'}`;
  return `${phases} · ${gates}`;
}

/**
 * `plan → port ⇉ merge → validate` — the definition's phase chain as one
 * line. Fan-out and loop markers are not in the listing projection, so the
 * chain renders phase ids only; the studio thread owns the full shape.
 */
export function workflowChainSummary(definition: WorkflowDefinitionListing): string {
  const ids = (definition.phases ?? []).map((phase) => phase.id).filter(Boolean);
  if (ids.length === 0) return '';
  return ids.join(' → ');
}

// Bare relative age for workflow meta copy ("6m", "7h", "3d"). Callers append
// "ago" only where the spec copy carries it (`spawned 6m ago`, `finished 2h
// ago`) and use the bare form elsewhere (`parked 7h`); "<1m" composes in both
// forms, unlike a "just now" phrasing.
export function workflowAge(timestampMs: number, nowMs = Date.now()): string {
  const minutes = Math.max(0, Math.floor((nowMs - timestampMs) / 60_000));
  if (minutes < 1) return '<1m';
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

/** "in 3h 40m" / "in 45s" — the countdown on an automation row. */
export function workflowCountdown(timestampMs: number, nowMs = Date.now()): string {
  const seconds = Math.max(0, Math.round((timestampMs - nowMs) / 1000));
  if (seconds < 60) return `in ${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `in ${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const remainder = minutes % 60;
  if (hours < 24) return remainder > 0 ? `in ${hours}h ${remainder}m` : `in ${hours}h`;
  return `in ${Math.floor(hours / 24)}d`;
}

export function formatWorkflowCost(costUsd: number | undefined): string {
  if (typeof costUsd !== 'number' || !Number.isFinite(costUsd) || costUsd <= 0) return '';
  return `$${costUsd.toFixed(2)}`;
}

/** `·`-joined metadata line (R4). Empty fragments drop out. */
export function workflowMetaLine(fragments: readonly (string | undefined | null)[]): string {
  return fragments.filter((fragment): fragment is string => Boolean(fragment && fragment.trim())).join(' · ');
}

export function patchWorkflowItems(items: WorkItem[], event: WorkflowItemStateEvent): WorkItem[] {
  return items.map((item) => item.id === event.itemId
    ? { ...item, state: event.to, reason: event.reason ?? '' } as WorkItem
    : item);
}

/**
 * A run tree's stop request changed (D36). It is a separate patch from the
 * state one because it is a separate fact: the run did not transition, it
 * acquired (or lost) an appointment to stop at its next call boundary.
 */
export function patchWorkflowSoftStop(items: WorkItem[], itemId: string, armed: boolean): WorkItem[] {
  return items.map((item) => item.id === itemId ? { ...item, softStop: armed } as WorkItem : item);
}

// A disposition receipt is the resolution record for any state — the
// frontend mirror of the store's unresolved predicate. Cancellation
// resolves without a receipt.
export function isWorkflowResolved(item: WorkItem): boolean {
  return item.state === 'cancelled' || parseWorkflowDisposition(item.disposition) !== null;
}

/** A run at rest that a human still owes something. */
export function isWorkflowParked(item: WorkItem): boolean {
  if (isWorkflowResolved(item)) return false;
  return item.state === 'needs-human' || item.state === 'failed' || item.state === 'done';
}

/** A root run — the unit of attention. Child runs surface through their root. */
export function isWorkflowRootRun(item: WorkItem): boolean {
  return !item.parentItemId;
}

/**
 * The amber badge count (§6): root runs a human must unblock. R1 keeps
 * done-awaiting-disposition out of it — that state counts neutrally and never
 * turns amber — even though the sweep set below does include it.
 */
export function workflowAttentionCount(items: readonly WorkItem[]): number {
  let count = 0;
  for (const item of items) {
    if (!isWorkflowRootRun(item) || isWorkflowResolved(item)) continue;
    if (item.state === 'needs-human' || item.state === 'failed') count += 1;
  }
  return count;
}

export type WorkflowRunSection = 'attention' | 'running' | 'recent';

export function workflowRunSection(item: WorkItem): WorkflowRunSection {
  if (isWorkflowResolved(item)) return 'recent';
  if (item.state === 'needs-human' || item.state === 'failed') return 'attention';
  if (item.state === 'running') return 'running';
  // `done` awaiting disposition rests in the attention column without an amber
  // signal — it is work waiting on a decision, not work in flight.
  return 'attention';
}

export interface WorkflowProjectGroup {
  projectId: string;
  projectName: string;
  attention: WorkItem[];
  running: WorkItem[];
  recent: WorkItem[];
}

function attentionRank(item: WorkItem): number {
  if (item.state === 'needs-human') return 0;
  if (item.state === 'failed') return 1;
  return 2; // done awaiting disposition
}

function restedAt(item: WorkItem): number {
  return item.endedAt || item.startedAt || item.createdAt;
}

/**
 * Project-grouped home content (§3.2). Only root runs are listed — a child
 * run is a node of its parent's tree, never a row of its own. Groups come
 * back ordered by project name; a project with no runs is still returned so
 * the caller can decide whether its definitions/automations justify a group.
 */
export function groupWorkflowRunsByProject(
  items: readonly WorkItem[],
  projectNames: ReadonlyMap<string, string>,
  projectFilter = '',
): WorkflowProjectGroup[] {
  const groups = new Map<string, WorkflowProjectGroup>();
  const ensure = (projectId: string): WorkflowProjectGroup => {
    let group = groups.get(projectId);
    if (!group) {
      group = {
        projectId,
        projectName: projectNames.get(projectId) ?? projectId,
        attention: [],
        running: [],
        recent: [],
      };
      groups.set(projectId, group);
    }
    return group;
  };
  for (const projectId of projectNames.keys()) {
    if (projectFilter && projectId !== projectFilter) continue;
    ensure(projectId);
  }
  for (const item of items) {
    if (!isWorkflowRootRun(item)) continue;
    if (projectFilter && item.projectId !== projectFilter) continue;
    if (!item.projectId) continue;
    ensure(item.projectId)[workflowRunSection(item)].push(item);
  }
  for (const group of groups.values()) {
    group.attention.sort((left, right) =>
      attentionRank(left) - attentionRank(right)
      || restedAt(left) - restedAt(right)
      || left.id.localeCompare(right.id));
    group.running.sort((left, right) =>
      (left.startedAt || left.createdAt) - (right.startedAt || right.createdAt)
      || left.id.localeCompare(right.id));
    group.recent.sort((left, right) =>
      restedAt(right) - restedAt(left) || left.id.localeCompare(right.id));
  }
  return [...groups.values()].sort((left, right) =>
    left.projectName.localeCompare(right.projectName) || left.projectId.localeCompare(right.projectId));
}

/**
 * The sweep set (§4.4): every parked root run — all `needs-human` reasons
 * plus `failed` plus done-awaiting-disposition — oldest-parked first,
 * app-wide, respecting the project filter. Runs resolved during this session
 * stay in the set so the receipt has somewhere to render before the sweep
 * moves on.
 */
export function workflowSweepItems(
  items: readonly WorkItem[],
  receipts: ReadonlyMap<string, WorkflowResolvedReceipt>,
  projectFilter = '',
): WorkItem[] {
  return items
    .filter((item) => isWorkflowRootRun(item)
      && (!projectFilter || item.projectId === projectFilter)
      && (isWorkflowParked(item) || receipts.has(item.id)))
    .sort((left, right) => restedAt(left) - restedAt(right) || left.id.localeCompare(right.id));
}

export interface WorkflowSessionSummary {
  count: number;
  costUsd: number;
  /** `3 approved · 1 merged` — receipt kinds in a fixed, readable order. */
  fragments: string;
}

const RECEIPT_LABELS: readonly (readonly [WorkflowResolvedReceipt['kind'], string])[] = [
  ['approved', 'approved'],
  ['answered', 'answered'],
  ['restarted', 'restarted'],
  ['handed-off', 'handed off'],
  ['merged', 'merged'],
  ['pr', 'PR created'],
  ['discarded', 'discarded'],
];

/** The all-clear summary (§4.4): what this sweep session actually resolved. */
export function workflowSessionSummary(
  receipts: ReadonlyMap<string, WorkflowResolvedReceipt>,
): WorkflowSessionSummary {
  const counts = new Map<string, number>();
  let costUsd = 0;
  for (const receipt of receipts.values()) {
    counts.set(receipt.kind, (counts.get(receipt.kind) ?? 0) + 1);
    if (Number.isFinite(receipt.costUsd)) costUsd += receipt.costUsd;
  }
  return {
    count: receipts.size,
    costUsd,
    fragments: RECEIPT_LABELS
      .filter(([kind]) => (counts.get(kind) ?? 0) > 0)
      .map(([kind, label]) => `${counts.get(kind)} ${label}`)
      .join(' · '),
  };
}

export interface WorkflowSweepStep {
  itemId: string;
  index: number;
}

/**
 * Step the sweep cursor, wrapping. `anchorIndex` is the position the caller
 * last occupied — it survives the current run leaving the set, which is
 * exactly the auto-advance case. `skip` holds runs already resolved this
 * session: they stay in the set so their receipt can render, but the cursor
 * never lands on one again. Returns null when nothing is left to visit, which
 * is what pushes all-clear (§4.4).
 */
export function stepWorkflowSweep(
  items: readonly WorkItem[],
  anchorIndex: number,
  direction: -1 | 1,
  skip: ReadonlySet<string> = new Set(),
): WorkflowSweepStep | null {
  if (items.length === 0) return null;
  const from = anchorIndex < 0 ? (direction === 1 ? -1 : 0) : anchorIndex;
  for (let offset = 1; offset <= items.length; offset += 1) {
    const index = ((from + direction * offset) % items.length + items.length) % items.length;
    if (!skip.has(items[index].id)) return { itemId: items[index].id, index };
  }
  return null;
}

/**
 * Where a run sits in the sweep set, or the caller's remembered anchor when
 * the run has left it (acted on, then filtered out by its own resolution).
 */
export function workflowSweepPosition(
  items: readonly WorkItem[],
  itemId: string,
  anchorIndex: number,
): number {
  const index = items.findIndex((item) => item.id === itemId);
  if (index >= 0) return index;
  return anchorIndex >= 0 && anchorIndex < items.length ? anchorIndex : -1;
}

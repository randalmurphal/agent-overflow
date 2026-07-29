import { describe, expect, it } from 'vitest';
import type { WorkItem, WorkflowDefinitionListing, WorkflowResolvedReceipt } from '../types/workflow';
import {
  formatWorkflowCost,
  groupWorkflowRunsByProject,
  isWorkflowParked,
  isWorkflowResolved,
  isWorkflowRootRun,
  patchWorkflowItems,
  patchWorkflowSoftStop,
  stepWorkflowSweep,
  workflowAge,
  workflowAttentionCount,
  workflowChainSummary,
  workflowCountdown,
  workflowDefinitionMeta,
  workflowMetaLine,
  workflowRunSection,
  workflowSessionSummary,
  workflowSweepItems,
  workflowSweepPosition,
} from './workflowData';

function item(id: string, state: string, endedAt: number, extra: Partial<WorkItem> = {}): WorkItem {
  return { id, state, endedAt, createdAt: endedAt, projectId: 'p', ...extra } as WorkItem;
}

const MERGED = '{"action":"merged","policy":"manual","at":1}';

describe('workflow run projections', () => {
  it('patches item state from events without mutating other rows', () => {
    const rows = [item('a', 'running', 1), item('b', 'running', 2)];
    const patched = patchWorkflowItems(rows, {
      itemId: 'a', projectId: 'p', from: 'running', to: 'needs-human', reason: 'gate',
    });
    expect(patched[0]).toMatchObject({ state: 'needs-human', reason: 'gate' });
    expect(patched[1]).toBe(rows[1]);
  });

  it('patches the stop request in both directions and leaves the run state alone', () => {
    const rows = [item('a', 'running', 1, { softStop: true }), item('b', 'running', 2)];
    const armed = patchWorkflowSoftStop(rows, 'b', true);
    expect(armed[1]).toMatchObject({ state: 'running', softStop: true });
    expect(armed[0]).toBe(rows[0]);
    const cleared = patchWorkflowSoftStop(armed, 'a', false);
    expect(cleared[0]).toMatchObject({ state: 'running', softStop: false });
    expect(patchWorkflowSoftStop(rows, 'missing', true)).toEqual(rows);
  });

  it('resolves any disposition-bearing run out of the parked and sweep sets', () => {
    const discardedFailure = item('discarded-failed', 'failed', 40, { disposition: MERGED });
    expect(isWorkflowParked(discardedFailure)).toBe(false);
    expect(isWorkflowParked(item('failed', 'failed', 41))).toBe(true);
    expect(isWorkflowParked(item('running', 'running', 41))).toBe(false);
    expect(isWorkflowResolved(discardedFailure)).toBe(true);
    expect(isWorkflowResolved(item('cancelled', 'cancelled', 42))).toBe(true);
    expect(isWorkflowResolved(item('done', 'done', 43))).toBe(false);
    expect(workflowSweepItems([discardedFailure, item('failed', 'failed', 41)], new Map())
      .map((entry) => entry.id)).toEqual(['failed']);
  });

  it('counts only unresolved root runs a human must unblock, never done-awaiting-disposition', () => {
    const rows = [
      item('gate', 'needs-human', 1),
      item('failed', 'failed', 2),
      item('done', 'done', 3),
      item('resolved', 'needs-human', 4, { disposition: MERGED }),
      item('child', 'needs-human', 5, { parentItemId: 'gate' }),
      item('running', 'running', 6),
    ];
    expect(workflowAttentionCount(rows)).toBe(2);
    expect(isWorkflowRootRun(rows[4])).toBe(false);
  });

  it('sections a done-awaiting-disposition run with attention, not with history', () => {
    expect(workflowRunSection(item('a', 'done', 1))).toBe('attention');
    expect(workflowRunSection(item('a', 'done', 1, { disposition: MERGED }))).toBe('recent');
    expect(workflowRunSection(item('a', 'running', 1))).toBe('running');
    expect(workflowRunSection(item('a', 'failed', 1))).toBe('attention');
    expect(workflowRunSection(item('a', 'cancelled', 1))).toBe('recent');
  });
});

describe('home grouping', () => {
  const names = new Map([['p1', 'Beta'], ['p2', 'Alpha']]);

  it('groups root runs by project, ordered by project name, and drops child runs', () => {
    const rows = [
      item('p1-attention', 'needs-human', 5, { projectId: 'p1' }),
      item('p1-child', 'needs-human', 6, { projectId: 'p1', parentItemId: 'p1-attention' }),
      item('p2-running', 'running', 7, { projectId: 'p2', startedAt: 7 }),
      item('p2-recent', 'done', 8, { projectId: 'p2', disposition: MERGED }),
    ];
    const groups = groupWorkflowRunsByProject(rows, names);
    expect(groups.map((group) => group.projectId)).toEqual(['p2', 'p1']);
    expect(groups[1].attention.map((entry) => entry.id)).toEqual(['p1-attention']);
    expect(groups[0].running.map((entry) => entry.id)).toEqual(['p2-running']);
    expect(groups[0].recent.map((entry) => entry.id)).toEqual(['p2-recent']);
  });

  it('orders attention by needs-human, then failed, then done-awaiting-disposition, oldest first', () => {
    const rows = [
      item('done', 'done', 1, { projectId: 'p1' }),
      item('failed', 'failed', 2, { projectId: 'p1' }),
      item('newer-human', 'needs-human', 9, { projectId: 'p1' }),
      item('older-human', 'needs-human', 3, { projectId: 'p1' }),
    ];
    const group = groupWorkflowRunsByProject(rows, names, 'p1')[0];
    expect(group.attention.map((entry) => entry.id)).toEqual(['older-human', 'newer-human', 'failed', 'done']);
  });

  it('honours the project filter on both the runs and the empty groups', () => {
    const rows = [item('a', 'running', 1, { projectId: 'p1' }), item('b', 'running', 2, { projectId: 'p2' })];
    const groups = groupWorkflowRunsByProject(rows, names, 'p2');
    expect(groups.map((group) => group.projectId)).toEqual(['p2']);
    expect(groups[0].running.map((entry) => entry.id)).toEqual(['b']);
  });
});

describe('sweep cursor', () => {
  const rows = [
    item('new', 'failed', 30),
    item('done', 'done', 20),
    item('old', 'needs-human', 10),
    item('disposed', 'done', 5, { disposition: MERGED }),
  ];
  const receipts = new Map<string, WorkflowResolvedReceipt>([
    ['done', { itemId: 'done', kind: 'merged', message: 'Merged', costUsd: 1 }],
  ]);

  it('orders parked runs oldest first and keeps this session\'s resolutions in the set', () => {
    expect(workflowSweepItems(rows, receipts).map((entry) => entry.id)).toEqual(['old', 'done', 'new']);
    expect(workflowSweepItems(rows, new Map()).map((entry) => entry.id)).toEqual(['old', 'done', 'new']);
  });

  it('steps in both directions, wraps, and never lands on a resolved run twice', () => {
    const sweep = workflowSweepItems(rows, receipts);
    const skip = new Set(receipts.keys());
    expect(stepWorkflowSweep(sweep, 0, 1, skip)).toEqual({ itemId: 'new', index: 2 });
    expect(stepWorkflowSweep(sweep, 0, -1, skip)).toEqual({ itemId: 'new', index: 2 });
    expect(stepWorkflowSweep(sweep, 2, 1, skip)).toEqual({ itemId: 'old', index: 0 });
    expect(stepWorkflowSweep(sweep, -1, 1)).toEqual({ itemId: 'old', index: 0 });
    expect(stepWorkflowSweep(sweep, -1, -1)).toEqual({ itemId: 'new', index: 2 });
  });

  it('returns null — the all-clear signal — once every run is resolved', () => {
    const sweep = workflowSweepItems(rows, receipts);
    expect(stepWorkflowSweep(sweep, 0, 1, new Set(['old', 'done', 'new']))).toBeNull();
    expect(stepWorkflowSweep([], 0, 1)).toBeNull();
  });

  it('falls back to the remembered anchor when the run has left the set', () => {
    const sweep = workflowSweepItems(rows, receipts);
    expect(workflowSweepPosition(sweep, 'done', -1)).toBe(1);
    expect(workflowSweepPosition(sweep, 'gone', 1)).toBe(1);
    expect(workflowSweepPosition(sweep, 'gone', 99)).toBe(-1);
    expect(workflowSweepPosition(sweep, 'gone', -1)).toBe(-1);
  });

  it('summarises the session by receipt kind and reviewed cost', () => {
    const summary = workflowSessionSummary(new Map<string, WorkflowResolvedReceipt>([
      ['a', { itemId: 'a', kind: 'approved', message: '', costUsd: 1.5 }],
      ['b', { itemId: 'b', kind: 'approved', message: '', costUsd: 0.5 }],
      ['c', { itemId: 'c', kind: 'merged', message: '', costUsd: 1 }],
    ]));
    expect(summary).toEqual({ count: 3, costUsd: 3, fragments: '2 approved · 1 merged' });
    expect(workflowSessionSummary(new Map())).toEqual({ count: 0, costUsd: 0, fragments: '' });
  });
});

describe('workflow copy helpers', () => {
  it('renders bare workflow ages that compose with and without "ago"', () => {
    const now = Date.now();
    expect(workflowAge(now - 30_000)).toBe('<1m');
    expect(workflowAge(now - 6 * 60_000)).toBe('6m');
    expect(workflowAge(now - 7 * 3_600_000)).toBe('7h');
    expect(workflowAge(now - 3 * 86_400_000)).toBe('3d');
  });

  it('counts down to an automation fire', () => {
    const now = 1_000_000;
    expect(workflowCountdown(now + 45_000, now)).toBe('in 45s');
    expect(workflowCountdown(now + 5 * 60_000, now)).toBe('in 5m');
    expect(workflowCountdown(now + 3 * 3_600_000 + 40 * 60_000, now)).toBe('in 3h 40m');
    expect(workflowCountdown(now + 4 * 3_600_000, now)).toBe('in 4h');
    expect(workflowCountdown(now - 1, now)).toBe('in 0s');
  });

  it('drops empty fragments from a `·` meta line and hides a zero cost', () => {
    expect(workflowMetaLine(['phase 1/3', '', undefined, null, '  ', '$1.00'])).toBe('phase 1/3 · $1.00');
    expect(formatWorkflowCost(0)).toBe('');
    expect(formatWorkflowCost(undefined)).toBe('');
    expect(formatWorkflowCost(Number.NaN)).toBe('');
    expect(formatWorkflowCost(3.104)).toBe('$3.10');
  });

  it('summarises a definition by phases, gates, and chain', () => {
    const definition = {
      id: 'port', name: 'Port', phaseCount: 3, humanGateCount: 1,
      phases: [{ id: 'plan' }, { id: 'port' }, { id: 'check' }],
    } as WorkflowDefinitionListing;
    expect(workflowDefinitionMeta(definition)).toBe('3 phases · 1 human gate');
    expect(workflowChainSummary(definition)).toBe('plan → port → check');
    expect(workflowDefinitionMeta({ phaseCount: 1, humanGateCount: 0 } as WorkflowDefinitionListing)).toBe('1 phase');
    expect(workflowChainSummary({ phases: [] } as unknown as WorkflowDefinitionListing)).toBe('');
  });
});

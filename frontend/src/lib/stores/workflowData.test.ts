import { describe, expect, it } from 'vitest';
import type { WorkItem, WorkflowDefinitionCatalog, WorkflowResolvedReceipt } from '../types/workflow';
import {
  mergeWorkflowProjectLoads,
  nextWorkflowSweepIndex,
  patchWorkflowItems,
  workflowSweepItems,
} from './workflowData';

function item(id: string, state: string, endedAt: number, disposition = ''): WorkItem {
  return { id, state, endedAt, disposition, createdAt: endedAt, sortPosition: endedAt } as WorkItem;
}

describe('workflow data reducers', () => {
  it('merges all-project fan-out results and costs', () => {
    const catalog = { workflows: [{ id: 'wf' }] } as WorkflowDefinitionCatalog;
    const merged = mergeWorkflowProjectLoads([
      { projectId: 'p1', items: [item('b', 'queued', 2)], costs: { b: 2 }, catalog },
      { projectId: 'p2', items: [item('a', 'running', 1)], costs: { a: 1 }, catalog },
    ]);
    expect(merged.items.map((entry) => entry.id)).toEqual(['a', 'b']);
    expect(merged.costs).toEqual({ a: 1, b: 2 });
    expect(merged.definitions.map((entry) => entry.projectId)).toEqual(['p1', 'p2']);
  });

  it('patches item state from events without mutating other rows', () => {
    const rows = [item('a', 'running', 1), item('b', 'queued', 2)];
    const patched = patchWorkflowItems(rows, {
      itemId: 'a', from: 'running', to: 'needs-human', reason: 'gate',
    });
    expect(patched[0]).toMatchObject({ state: 'needs-human', reason: 'gate' });
    expect(patched[1]).toBe(rows[1]);
  });

  it('orders parked runs oldest first, wraps, and skips session receipts', () => {
    const rows = [
      item('new', 'failed', 30),
      item('done', 'done', 20),
      item('old', 'needs-human', 10),
      item('disposed', 'done', 5, '{"action":"merged","policy":"manual","at":1}'),
    ];
    const receipts = new Map<string, WorkflowResolvedReceipt>([
      ['done', { itemId: 'done', kind: 'merged', message: 'Merged', costUsd: 1 }],
    ]);
    const sweep = workflowSweepItems(rows, receipts);
    expect(sweep.map((entry) => entry.id)).toEqual(['old', 'done', 'new']);
    expect(nextWorkflowSweepIndex(sweep, 0, -1, receipts, true)).toBe(2);
    expect(nextWorkflowSweepIndex(sweep, 0, 1, receipts, true)).toBe(2);
  });
});

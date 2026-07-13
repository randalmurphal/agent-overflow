import { describe, expect, it } from 'vitest';
import type { WorkItem, WorkflowDefinitionCatalog, WorkflowResolvedReceipt } from '../types/workflow';
import {
  mergeWorkflowProjectLoads,
  isWorkflowSidebarRun,
  loadWorkflowSidebar,
  nextWorkflowSweepIndex,
  patchWorkflowItems,
  workflowSweepItems,
  workflowSidebarRuns,
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

  it('loads all items once and fetches definitions for every known project', async () => {
    const listItems = async () => [
      { ...item('a', 'running', 1), projectId: 'p1' },
      { ...item('b', 'queued', 2), projectId: 'p2' },
      { ...item('resolved', 'done', 3, '{"action":"merged","policy":"manual","at":1}'), projectId: 'p3' },
      { ...item('cancelled', 'cancelled', 4), projectId: 'p4' },
    ];
    const definitionCalls: string[] = [];
    const loaded = await loadWorkflowSidebar(['p1', 'p2', 'p5'], {
      listItems,
      listDefinitions: async (projectId) => {
        definitionCalls.push(projectId);
        return { workflows: [{ id: `${projectId}-wf` }] } as WorkflowDefinitionCatalog;
      },
    });
    expect(definitionCalls.sort()).toEqual(['p1', 'p2', 'p5']);
    expect(loaded.items.map((entry) => entry.id)).toEqual(['a', 'b']);
    expect(loaded.definitions.map((entry) => entry.projectId).sort()).toEqual(['p1', 'p2', 'p5']);
  });

  it('keeps only live and awaiting-disposition sidebar runs in signal order', () => {
    const rows = [
      { ...item('queued', 'queued', 4), projectId: 'p' },
      { ...item('failed', 'failed', 2), projectId: 'p' },
      { ...item('new-attention', 'needs-human', 3), projectId: 'p' },
      { ...item('old-attention', 'needs-human', 1), projectId: 'p' },
      { ...item('running', 'running', 5), projectId: 'p' },
      { ...item('done', 'done', 6), projectId: 'p' },
      { ...item('resolved', 'done', 7, '{"action":"merged","policy":"manual","at":1}'), projectId: 'p' },
      { ...item('cancelled', 'cancelled', 8), projectId: 'p' },
    ];
    expect(workflowSidebarRuns(rows, 'p').map((entry) => entry.id)).toEqual([
      'old-attention', 'new-attention', 'failed', 'running', 'queued', 'done',
    ]);
    expect(isWorkflowSidebarRun(rows[6])).toBe(false);
    expect(isWorkflowSidebarRun(rows[7])).toBe(false);
  });
});

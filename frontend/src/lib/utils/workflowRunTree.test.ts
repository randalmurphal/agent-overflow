import { describe, expect, it } from 'vitest';
import type { WorkItemPhase, WorkItemUnit, WorkflowItemDetail } from '../types/workflow';
import {
  buildWorkflowRunTree,
  failedWorkflowUnit,
  failedWorkflowUnitInDetail,
  workflowDuration,
  workflowNodeTone,
} from './workflowRunTree';

const NOW = 1_000_000;

function phase(phaseId: string, attempt: number, status: string, startedAt: number, extra: Partial<WorkItemPhase> = {}): WorkItemPhase {
  return { itemId: 'run', phaseId, attempt, status, startedAt, ...extra } as WorkItemPhase;
}

function unit(unitId: string, unitIndex: number, status: string, extra: Partial<WorkItemUnit> = {}): WorkItemUnit {
  return {
    itemId: 'run', phaseId: 'port', attempt: 1, unitId, unitIndex,
    kind: 'unit', status, unitAttempt: 1, ...extra,
  } as WorkItemUnit;
}

function detail(over: Partial<WorkflowItemDetail>): WorkflowItemDetail {
  return {
    item: { id: 'run' }, checkPhaseIds: [], phases: [], units: [], children: [],
    outputs: {}, artifacts: [], usage: { costUsd: 0 },
    ...over,
  } as unknown as WorkflowItemDetail;
}

describe('buildWorkflowRunTree', () => {
  it('orders phase attempts by start and labels retries per attempt', () => {
    const nodes = buildWorkflowRunTree(detail({
      phases: [
        phase('check', 2, 'completed', 300),
        phase('plan', 1, 'completed', 100),
        phase('check', 1, 'failed', 200),
      ],
    }), NOW);
    expect(nodes.map((node) => node.label)).toEqual(['plan', 'check · attempt 1', 'check · attempt 2']);
    expect(nodes.map((node) => node.glyph)).toEqual(['✓', '✗', '✓']);
  });

  it('attaches units to the exact attempt that produced them, join last', () => {
    const nodes = buildWorkflowRunTree(detail({
      phases: [phase('port', 1, 'completed', 100), phase('port', 2, 'running', 200)],
      units: [
        unit('port-join', 9, 'done', { kind: 'join' }),
        unit('port-b', 1, 'running'),
        unit('port-a', 0, 'done'),
        unit('retry-a', 0, 'pending', { attempt: 2 }),
      ],
    }), NOW);
    expect(nodes[0].units.map((row) => row.unit.unitId)).toEqual(['port-a', 'port-b', 'port-join']);
    expect(nodes[0].units[2].isJoin).toBe(true);
    expect(nodes[0].units[2].label).toBe('port-join (join)');
    expect(nodes[1].units.map((row) => row.unit.unitId)).toEqual(['retry-a']);
  });

  it('names the provider a pending unit is waiting capacity on — the only visible bound', () => {
    const [node] = buildWorkflowRunTree(detail({
      phases: [phase('port', 1, 'running', 100)],
      units: [unit('port-a', 0, 'pending', { provider: 'codex' })],
    }), NOW);
    expect(node.units[0].meta).toBe('waiting on provider:codex');
    expect(node.units[0].glyph).toBe('○');
  });

  it('says what a dropped unit meant for the join, and counts unit retries', () => {
    const [node] = buildWorkflowRunTree(detail({
      phases: [phase('port', 1, 'running', 100)],
      units: [
        unit('port-a', 0, 'dropped'),
        unit('port-b', 1, 'failed', { unitAttempt: 2, startedAt: NOW - 60_000, endedAt: NOW }),
      ],
    }), NOW);
    expect(node.units[0].meta).toBe('dropped — join proceeded without it');
    expect(node.units[1].meta).toBe('×2 · 1m');
  });

  it('attaches child runs to the call attempt that invoked them, sorted stably', () => {
    const nodes = buildWorkflowRunTree(detail({
      phases: [phase('call', 1, 'completed', 100), phase('call', 2, 'running', 200)],
      children: [
        { itemId: 'child-b', parentPhaseId: 'call', parentAttempt: 1 },
        { itemId: 'child-a', parentPhaseId: 'call', parentAttempt: 1 },
        { itemId: 'child-c', parentPhaseId: 'call', parentAttempt: 2 },
      ] as WorkflowItemDetail['children'],
    }), NOW);
    expect(nodes[0].children.map((child) => child.itemId)).toEqual(['child-a', 'child-b']);
    expect(nodes[1].children.map((child) => child.itemId)).toEqual(['child-c']);
  });

  it('carries every openable thread through to its row', () => {
    const [node] = buildWorkflowRunTree(detail({
      phases: [phase('port', 1, 'running', 100, { threadId: 'thread-phase' })],
      units: [unit('port-a', 0, 'running', { threadId: 'thread-unit' })],
    }), NOW);
    expect(node.threadId).toBe('thread-phase');
    expect(node.units[0].threadId).toBe('thread-unit');
  });

  it('falls back to pending rather than guessing at an unknown status', () => {
    const [node] = buildWorkflowRunTree(detail({
      phases: [phase('port', 1, 'invented', 100)],
      units: [unit('port-a', 0, 'invented')],
    }), NOW);
    expect(node.signal).toBe('pending');
    expect(node.units[0].signal).toBe('pending');
  });
});

describe('failedWorkflowUnit', () => {
  const failing = detail({
    phases: [phase('port', 1, 'running', 100)],
    units: [unit('port-a', 0, 'done'), unit('port-b', 1, 'failed')],
  });

  it('picks the newest failed unit', () => {
    expect(failedWorkflowUnitInDetail(failing, NOW)?.unit.unitId).toBe('port-b');
  });

  it('falls back to a taken-over unit when nothing failed outright', () => {
    const takenOver = detail({
      phases: [phase('port', 1, 'running', 100)],
      units: [unit('port-a', 0, 'taken-over')],
    });
    expect(failedWorkflowUnitInDetail(takenOver, NOW)?.unit.unitId).toBe('port-a');
  });

  it('returns null for a run with no fan-out', () => {
    expect(failedWorkflowUnit(buildWorkflowRunTree(detail({ phases: [phase('plan', 1, 'completed', 1)] }), NOW))).toBeNull();
  });
});

describe('presentation helpers', () => {
  it('reserves colour for failure and attention (R1)', () => {
    expect(workflowNodeTone('failed')).toBe('text-error');
    expect(workflowNodeTone('parked')).toBe('text-warning');
    for (const signal of ['done', 'running', 'pending', 'dropped'] as const) {
      expect(workflowNodeTone(signal)).toBe('text-fg-muted');
    }
  });

  it('formats an elapsed span, using now for a run still going', () => {
    expect(workflowDuration(0, 0, NOW)).toBe('');
    expect(workflowDuration(NOW - 48_000, NOW, NOW)).toBe('48s');
    expect(workflowDuration(NOW - 12 * 60_000, NOW, NOW)).toBe('12m');
    expect(workflowDuration(NOW - 64 * 60_000, NOW, NOW)).toBe('1h 4m');
    expect(workflowDuration(NOW - 2 * 3_600_000, NOW, NOW)).toBe('2h');
    expect(workflowDuration(NOW - 30_000, 0, NOW)).toBe('30s');
  });
});

import { describe, expect, it } from 'vitest';
import type { Item } from '../types/models';
import { completionStatusFor, deriveTrayTasks } from './backgroundTray';

function makeItem(overrides: Partial<Item> = {}): Item {
  const createdAt = overrides.createdAt ?? 0;
  return {
    id: 'launch-1',
    threadId: 't',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'running',
    summary: 'tool',
    highlightedContent: '',
    isBackground: true,
    createdAt,
    updatedAt: overrides.updatedAt ?? createdAt,
    ...overrides,
  };
}

describe('completionStatusFor', () => {
  it('maps errored / declined / everything-else distinctly', () => {
    expect(completionStatusFor(makeItem({ status: 'errored' }))).toBe('errored');
    expect(completionStatusFor(makeItem({ status: 'declined' }))).toBe('declined');
    expect(completionStatusFor(makeItem({ status: 'completed' }))).toBe('completed');
    // Unknown status lands on the completed bucket rather than crashing;
    // the backend contract is clear but callers shouldn't hard-fault if
    // a new status appears before the frontend learns about it.
    expect(completionStatusFor(makeItem({ status: 'mystery' as Item['status'] }))).toBe(
      'completed',
    );
  });
});

describe('deriveTrayTasks', () => {
  const RETENTION_MS = 2_000;

  it('pairs launch with completion and emits the completion status', () => {
    const launch = makeItem({ id: 'L', status: 'running', createdAt: 100, updatedAt: 100 });
    const completion = makeItem({
      id: 'C',
      status: 'completed',
      completionOf: 'L',
      createdAt: 500,
      updatedAt: 500,
      isBackground: false,
    });
    const out = deriveTrayTasks([launch, completion], 600, RETENTION_MS);
    expect(out).toHaveLength(1);
    expect(out[0].rowId).toBe('L');
    expect(out[0].status).toBe('completed');
    expect(out[0].launch?.id).toBe('L');
    expect(out[0].completion?.id).toBe('C');
    expect(out[0].elapsedMs).toBe(500);
  });

  it('running launch without completion stays in the list and reports elapsed', () => {
    const launch = makeItem({ id: 'L', status: 'running', createdAt: 200 });
    const out = deriveTrayTasks([launch], 5_000, RETENTION_MS);
    expect(out).toHaveLength(1);
    expect(out[0].status).toBe('running');
    expect(out[0].elapsedMs).toBe(4_800);
    expect(out[0].completion).toBeNull();
  });

  it('drops the pair once the completion ages past retention', () => {
    const launch = makeItem({ id: 'L', status: 'running', createdAt: 0 });
    const completion = makeItem({
      id: 'C',
      status: 'completed',
      completionOf: 'L',
      createdAt: 1_000,
      isBackground: false,
    });
    // t=2000: exactly at retention boundary → dropped (>= retentionMs)
    expect(deriveTrayTasks([launch, completion], 3_000, RETENTION_MS)).toHaveLength(0);
    expect(deriveTrayTasks([launch, completion], 2_500, RETENTION_MS)).toHaveLength(1);
  });

  it('orphan completion (launch already pruned) still renders during retention', () => {
    const completion = makeItem({
      id: 'C',
      status: 'completed',
      completionOf: 'gone',
      createdAt: 100,
      isBackground: false,
    });
    const out = deriveTrayTasks([completion], 500, RETENTION_MS);
    expect(out).toHaveLength(1);
    expect(out[0].rowId).toBe('C');
    expect(out[0].launch).toBeNull();
    // No launch → no meaningful start time → no elapsed label.
    expect(out[0].elapsedMs).toBeNull();
  });

  it('sorts by max(launch.updatedAt, completion.updatedAt) so just-completed pairs bubble up', () => {
    const staleLaunch = makeItem({ id: 'stale', createdAt: 0, updatedAt: 0 });
    const freshLaunch = makeItem({ id: 'fresh', createdAt: 10, updatedAt: 10 });
    const freshCompletion = makeItem({
      id: 'fc',
      status: 'completed',
      completionOf: 'fresh',
      createdAt: 500,
      updatedAt: 500,
      isBackground: false,
    });
    const out = deriveTrayTasks([staleLaunch, freshLaunch, freshCompletion], 600, RETENTION_MS);
    expect(out.map((t) => t.rowId)).toEqual(['fresh', 'stale']);
  });

  it('picks the highest-createdAt completion when duplicates arrive out of order', () => {
    const launch = makeItem({ id: 'L' });
    const early = makeItem({
      id: 'C',
      status: 'errored',
      completionOf: 'L',
      createdAt: 100,
      isBackground: false,
    });
    const late = makeItem({
      id: 'C',
      status: 'completed',
      completionOf: 'L',
      createdAt: 200,
      isBackground: false,
    });
    // Even if `late` arrives first in the list, the higher createdAt wins.
    const out = deriveTrayTasks([launch, late, early], 300, RETENTION_MS);
    expect(out).toHaveLength(1);
    expect(out[0].status).toBe('completed');
    expect(out[0].completion?.createdAt).toBe(200);
  });

  it('ignores non-running launches (defensive — backend should filter but we verify)', () => {
    const done = makeItem({ id: 'D', status: 'completed' });
    expect(deriveTrayTasks([done], 10, RETENTION_MS)).toHaveLength(0);
  });
});

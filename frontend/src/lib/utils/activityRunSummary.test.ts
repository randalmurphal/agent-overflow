import { describe, expect, it } from 'vitest';
import { activityRunSummary } from './activityRunSummary';
import type { Item } from '../types/models';
import { makeItem } from '../../test/helpers/chat';

let seq = 0;
function tool(id: string, toolName: string, overrides: Partial<Item> = {}): Item {
  seq += 1;
  return makeItem({ id, itemIndex: seq, kind: 'tool_call', toolName, ...overrides });
}

function thinking(id: string): Item {
  seq += 1;
  return makeItem({ id, itemIndex: seq, kind: 'thinking' });
}

describe('counts', () => {
  it('aggregates by tool display name, count-descending', () => {
    const { counts } = activityRunSummary([
      tool('a', 'Bash'),
      tool('b', 'Read'),
      tool('c', 'Bash'),
      tool('d', 'Bash'),
      tool('e', 'Read'),
    ]);

    expect(counts.entries).toEqual([
      { label: 'Bash', count: 3 },
      { label: 'Read', count: 2 },
    ]);
    expect(counts.total).toBe(5);
  });

  it('breaks a count tie alphabetically so the line is stable', () => {
    const { counts } = activityRunSummary([tool('a', 'Write'), tool('b', 'Edit')]);

    expect(counts.entries.map((e) => e.label)).toEqual(['Edit', 'Write']);
  });

  it('sorts thinking last regardless of count', () => {
    const { counts } = activityRunSummary([
      thinking('th1'),
      thinking('th2'),
      thinking('th3'),
      tool('a', 'Bash'),
    ]);

    expect(counts.entries.map((e) => e.label)).toEqual(['Bash', 'thinking']);
  });

  it('pairs a completion with its call instead of double-counting', () => {
    const { counts } = activityRunSummary([
      tool('t1', 'Bash'),
      makeItem({ id: 'c1', kind: 'tool_completion', toolName: 'Bash', completionOf: 't1' }),
    ]);

    expect(counts.entries).toEqual([{ label: 'Bash', count: 1 }]);
    expect(counts.total).toBe(1);
  });

  it('counts an orphan completion, so a head-trimmed run still reports honestly', () => {
    // The call was trimmed out of the window; the completion is all that is
    // left of that tool, and dropping it would under-report the run.
    const { counts } = activityRunSummary([
      makeItem({ id: 'c1', kind: 'tool_completion', toolName: 'Bash', completionOf: 'gone' }),
    ]);

    expect(counts.entries).toEqual([{ label: 'Bash', count: 1 }]);
  });

  it('falls back to a generic label for an unnamed tool', () => {
    const { counts } = activityRunSummary([tool('t1', '   ')]);

    expect(counts.entries).toEqual([{ label: 'tool', count: 1 }]);
  });
});

describe('attention state', () => {
  it('flags a failed member so a chip cannot hide it', () => {
    const summary = activityRunSummary([
      tool('t1', 'Bash', { status: 'errored' }),
      tool('t2', 'Read'),
    ]);

    expect(summary.hasFailure).toBe(true);
  });

  it('treats a killed member as a failure', () => {
    expect(activityRunSummary([tool('t1', 'Bash', { status: 'killed' })]).hasFailure).toBe(true);
  });

  it('does not treat a declined member as a failure — that was a user decision', () => {
    expect(activityRunSummary([tool('t1', 'Bash', { status: 'declined' })]).hasFailure).toBe(false);
  });

  it('names the newest running member', () => {
    const summary = activityRunSummary([
      tool('t1', 'Read', { status: 'running' }),
      tool('t2', 'Bash', { status: 'running' }),
    ]);

    expect(summary.runningLabel).toBe('Bash');
  });

  it('counts a running member that is also a paired completion target', () => {
    // The pairing skip must not skip the status scan too: a completion
    // arriving for a still-streaming call is exactly when the chip most
    // needs to say something is going on.
    const summary = activityRunSummary([
      tool('t1', 'Bash'),
      makeItem({
        id: 'c1',
        kind: 'tool_completion',
        toolName: 'Bash',
        completionOf: 't1',
        status: 'running',
      }),
    ]);

    expect(summary.runningLabel).toBe('Bash');
    expect(summary.counts.total).toBe(1);
  });

  it('reports no running label once everything settles', () => {
    expect(activityRunSummary([tool('t1', 'Bash')]).runningLabel).toBeNull();
  });

  it('says nothing about an empty run', () => {
    expect(activityRunSummary([])).toEqual({
      counts: { entries: [], total: 0 },
      hasFailure: false,
      runningLabel: null,
    });
  });
});

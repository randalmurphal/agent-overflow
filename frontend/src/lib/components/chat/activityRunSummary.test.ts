import { describe, expect, it } from 'vitest';
import { activityRunSummary } from './activityRunSummary';
import type { Item } from '../../types/models';
import { makeItem } from '../../../test/helpers/chat';

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
    ], 'claude');

    expect(counts.entries.map(({ label, count, icon }) => ({ label, count, icon }))).toEqual([
      { label: 'Bash', count: 3, icon: 'terminal' },
      { label: 'Read', count: 2, icon: 'eye' },
    ]);
    expect(counts.total).toBe(5);
  });

  it('breaks a count tie alphabetically so the line is stable', () => {
    const { counts } = activityRunSummary(
      [tool('a', 'Write'), tool('b', 'Edit')],
      'claude',
    );

    expect(counts.entries.map((e) => e.label)).toEqual(['Edit', 'Write']);
  });

  it('counts absorbed notification bells, ambient-ranked between tools and thinking', () => {
    seq += 1;
    const bell = makeItem({ id: 'bell', itemIndex: seq, kind: 'notification' });
    seq += 1;
    const bell2 = makeItem({ id: 'bell2', itemIndex: seq, kind: 'notification' });
    const { counts } = activityRunSummary([
      bell,
      tool('a', 'Bash'),
      thinking('th'),
      bell2,
    ], 'claude');

    expect(counts.entries.map(({ label, count }) => ({ label, count }))).toEqual([
      { label: 'Bash', count: 1 },
      // Pluralized: "2 notification" reads as a typo where "2 thinking" does not.
      { label: 'notifications', count: 2 },
      { label: 'thinking', count: 1 },
    ]);
  });

  it('keeps the singular label for one bell', () => {
    seq += 1;
    const bell = makeItem({ id: 'bell', itemIndex: seq, kind: 'notification' });
    const { counts } = activityRunSummary([tool('a', 'Bash'), bell], 'claude');

    expect(counts.entries.map(({ label }) => label)).toEqual(['Bash', 'notification']);
  });

  it('sorts thinking last regardless of count', () => {
    const { counts } = activityRunSummary([
      thinking('th1'),
      thinking('th2'),
      thinking('th3'),
      tool('a', 'Bash'),
    ], 'claude');

    expect(counts.entries.map((e) => e.label)).toEqual(['Bash', 'thinking']);
  });

  it('pairs a completion with its call instead of double-counting', () => {
    const { counts } = activityRunSummary([
      tool('t1', 'Bash'),
      makeItem({ id: 'c1', kind: 'tool_completion', toolName: 'Bash', completionOf: 't1' }),
    ], 'claude');

    expect(counts.entries.map(({ label, count }) => ({ label, count }))).toEqual([
      { label: 'Bash', count: 1 },
    ]);
    expect(counts.total).toBe(1);
  });

  it('counts an orphan completion, so a head-trimmed run still reports honestly', () => {
    // The call was trimmed out of the window; the completion is all that is
    // left of that tool, and dropping it would under-report the run.
    const { counts } = activityRunSummary([
      makeItem({ id: 'c1', kind: 'tool_completion', toolName: 'Bash', completionOf: 'gone' }),
    ], 'claude');

    expect(counts.entries.map(({ label, count }) => ({ label, count }))).toEqual([
      { label: 'Bash', count: 1 },
    ]);
  });

  it('capitalizes the generic label for an unnamed tool', () => {
    const { counts } = activityRunSummary([tool('t1', '   ')], 'claude');

    expect(counts.entries.map(({ label, count }) => ({ label, count }))).toEqual([
      { label: 'Tool', count: 1 },
    ]);
  });

  it('preserves and capitalizes Claude native tool names', () => {
    const { counts } = activityRunSummary([
      tool('a', 'ScheduleWakeup'),
      tool('b', 'advisor'),
    ], 'claude');

    expect(counts.entries.map((entry) => entry.label)).toEqual(['Advisor', 'ScheduleWakeup']);
  });

  it('aliases and capitalizes the Codex run from the regression report', () => {
    const items: Item[] = [
      makeItem({
        id: 'agent-result',
        itemIndex: 0,
        kind: 'tool_completion',
        toolName: 'collab_agent',
        completionOf: 'launch-outside-run',
      }),
      tool('edit-1', 'file_change'),
      tool('command-1', 'command_execution'),
      tool('command-2', 'command_execution'),
      tool('edit-2', 'file_change'),
      tool('command-3', 'command_execution'),
      tool('command-4', 'command_execution'),
      tool('command-5', 'command_execution'),
      makeItem({
        id: 'terminal-wait',
        itemIndex: 8,
        kind: 'terminal_interaction',
        toolName: '',
      }),
    ];

    const { counts } = activityRunSummary(items, 'codex');

    expect(counts.entries.map(({ label, count, icon }) => ({ label, count, icon }))).toEqual([
      { label: 'Bash', count: 5, icon: 'terminal' },
      { label: 'Edit', count: 2, icon: 'file' },
      { label: 'Agent', count: 1, icon: 'robot' },
      { label: 'Wait', count: 1, icon: 'clock' },
    ]);
  });

  it('merges Codex wire aliases by their presented identity', () => {
    const { counts } = activityRunSummary([
      tool('command-camel', 'commandExecution'),
      tool('command-snake', 'command_execution'),
      tool('file-camel', 'fileChange'),
      tool('file-snake', 'file_change'),
    ], 'codex');

    expect(counts.entries.map(({ label, count }) => ({ label, count }))).toEqual([
      { label: 'Bash', count: 2 },
      { label: 'Edit', count: 2 },
    ]);
  });

  it('counts projected file rows rather than Codex fileChange envelopes', () => {
    const { counts } = activityRunSummary([
      tool('multi-rich', 'file_change', {
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          inlineDiff: {
            totalFiles: 4,
            files: [
              { path: '/repo/a' },
              { path: '/repo/b' },
              { path: '/repo/c' },
              { path: '/repo/d' },
            ],
          },
        }),
      }),
      tool('single', 'file_change'),
      tool('multi-fallback', 'file_change', {
        meta: JSON.stringify({ input: { files: ['/repo/e', '/repo/f'] } }),
      }),
      tool('command', 'command_execution'),
    ], 'codex');

    expect(counts.entries.map(({ label, count }) => ({ label, count }))).toEqual([
      { label: 'Edit', count: 7 },
      { label: 'Bash', count: 1 },
    ]);
    expect(counts.total).toBe(8);
  });

  it('keeps MCP uppercase in a Codex header', () => {
    const { counts } = activityRunSummary([tool('mcp', 'MCP/lookup')], 'codex');

    expect(counts.entries[0].label).toBe('MCP');
  });

  it('keeps agent waiting distinct from a terminal wait', () => {
    const summary = activityRunSummary([
      makeItem({
        id: 'terminal-wait',
        itemIndex: 0,
        kind: 'terminal_interaction',
      }),
      tool('agent-wait', 'wait_agent', { status: 'running' }),
    ], 'codex');

    expect(summary.counts.entries.map(({ label, icon }) => ({ label, icon }))).toEqual([
      { label: 'Wait', icon: 'clock' },
      { label: 'Waiting', icon: 'robot' },
    ]);
    expect(summary.runningLabel).toBe('Waiting');
  });
});

describe('attention state', () => {
  it('flags a failed member so a chip cannot hide it', () => {
    const summary = activityRunSummary([
      tool('t1', 'Bash', { status: 'errored' }),
      tool('t2', 'Read'),
    ], 'claude');

    expect(summary.hasFailure).toBe(true);
  });

  it('treats a killed member as a failure', () => {
    expect(activityRunSummary(
      [tool('t1', 'Bash', { status: 'killed' })],
      'claude',
    ).hasFailure).toBe(true);
  });

  it('does not treat a declined member as a failure — that was a user decision', () => {
    expect(activityRunSummary(
      [tool('t1', 'Bash', { status: 'declined' })],
      'claude',
    ).hasFailure).toBe(false);
  });

  it('names the newest running member', () => {
    const summary = activityRunSummary([
      tool('t1', 'Read', { status: 'running' }),
      tool('t2', 'Bash', { status: 'running' }),
    ], 'claude');

    expect(summary.runningLabel).toBe('Bash');
  });

  it('lets a completion settle an immutable detached-agent launch', () => {
    const summary = activityRunSummary([
      tool('agent-launch', 'Agent', { status: 'running' }),
      makeItem({
        id: 'complete:agent-launch',
        kind: 'tool_completion',
        toolName: 'Agent',
        completionOf: 'agent-launch',
        status: 'completed',
      }),
    ], 'claude');

    expect(summary.runningLabel).toBeNull();
    expect(summary.counts.total).toBe(1);
  });

  it('takes failure state from the completion instead of its immutable launch', () => {
    const summary = activityRunSummary([
      tool('agent-launch', 'Agent', { status: 'running' }),
      makeItem({
        id: 'complete:agent-launch',
        kind: 'tool_completion',
        toolName: 'Agent',
        completionOf: 'agent-launch',
        status: 'errored',
      }),
    ], 'claude');

    expect(summary.runningLabel).toBeNull();
    expect(summary.hasFailure).toBe(true);
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
    ], 'claude');

    expect(summary.runningLabel).toBe('Bash');
    expect(summary.counts.total).toBe(1);
  });

  it('reports no running label once everything settles', () => {
    expect(activityRunSummary([tool('t1', 'Bash')], 'claude').runningLabel).toBeNull();
  });

  it('aliases a running Codex tool name', () => {
    const summary = activityRunSummary([
      tool('t1', 'command_execution', { status: 'running' }),
    ], 'codex');

    expect(summary.runningLabel).toBe('Bash');
  });

  it('says nothing about an empty run', () => {
    expect(activityRunSummary([], 'claude')).toEqual({
      counts: { entries: [], total: 0 },
      hasFailure: false,
      runningLabel: null,
    });
  });
});

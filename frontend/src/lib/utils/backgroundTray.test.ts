import { describe, expect, it, vi } from 'vitest';
import type { Item } from '../types/models';
import { __resetParseJsonObjectCacheForTest } from './parseJsonObject';
import {
  completionStatusFor,
  deriveTrayTasks,
  extractClaudeTaskID,
  formatElapsed,
  isCodexStoppableTask,
  isCodexSubagentTask,
  statusClass,
  statusLabel,
  trayRowStopTarget,
  trayTaskLabel,
  type TrayTask,
} from './backgroundTray';

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
    isBackground: true,
    createdAt,
    updatedAt: overrides.updatedAt ?? createdAt,
    ...overrides,
  };
}

function makeTrayTask(overrides: Partial<TrayTask> = {}): TrayTask {
  const launch = overrides.launch ?? makeItem({ id: 'L' });
  return {
    rowId: launch.id,
    anchor: launch,
    launch,
    completion: null,
    status: 'running',
    elapsedMs: 0,
    ...overrides,
  };
}

describe('completionStatusFor', () => {
  it('maps errored / declined / killed / everything-else distinctly', () => {
    expect(completionStatusFor(makeItem({ status: 'errored' }))).toBe('errored');
    expect(completionStatusFor(makeItem({ status: 'declined' }))).toBe('declined');
    // `killed` is a distinct terminal — user-initiated stop. Must NOT
    // collapse into `errored` (which would paint it red) or `completed`
    // (which would paint it green).
    expect(completionStatusFor(makeItem({ status: 'killed' }))).toBe('killed');
    expect(completionStatusFor(makeItem({ status: 'completed' }))).toBe('completed');
    // Unknown status lands on the completed bucket rather than crashing;
    // the backend contract is clear but callers shouldn't hard-fault if
    // a new status appears before the frontend learns about it.
    expect(completionStatusFor(makeItem({ status: 'mystery' as Item['status'] }))).toBe(
      'completed',
    );
  });
});

describe('extractClaudeTaskID', () => {
  it('returns the task_id from a well-formed meta JSON', () => {
    const item = makeItem({ meta: JSON.stringify({ task_id: 'tsk-42' }) });
    expect(extractClaudeTaskID(item)).toBe('tsk-42');
  });

  it('tolerates extra meta fields (triage merges unrelated data in)', () => {
    const item = makeItem({ meta: JSON.stringify({ task_id: 'tsk-99', other: 'x' }) });
    expect(extractClaudeTaskID(item)).toBe('tsk-99');
  });

  it('uses the shared JSON cache for repeated task-id extraction', () => {
    __resetParseJsonObjectCacheForTest();
    const item = makeItem({ meta: JSON.stringify({ task_id: 'tsk-cache' }) });
    const parseSpy = vi.spyOn(JSON, 'parse');

    try {
      expect(extractClaudeTaskID(item)).toBe('tsk-cache');
      expect(extractClaudeTaskID(item)).toBe('tsk-cache');
      expect(parseSpy).toHaveBeenCalledTimes(1);
    } finally {
      parseSpy.mockRestore();
    }
  });

  it('returns null when meta is missing or empty', () => {
    expect(extractClaudeTaskID(makeItem({ meta: undefined }))).toBeNull();
    expect(extractClaudeTaskID(makeItem({ meta: '' }))).toBeNull();
  });

  it('returns null when meta parses but has no task_id', () => {
    const item = makeItem({ meta: JSON.stringify({ other: 'x' }) });
    expect(extractClaudeTaskID(item)).toBeNull();
  });

  it('returns null when task_id is the wrong type', () => {
    const item = makeItem({ meta: JSON.stringify({ task_id: 42 }) });
    expect(extractClaudeTaskID(item)).toBeNull();
  });

  it('returns null when meta is malformed JSON', () => {
    const item = makeItem({ meta: '{not:json' });
    expect(extractClaudeTaskID(item)).toBeNull();
  });

  it('returns null when task_id is the empty string', () => {
    const item = makeItem({ meta: JSON.stringify({ task_id: '' }) });
    expect(extractClaudeTaskID(item)).toBeNull();
  });
});

describe('isCodexSubagentTask', () => {
  it('detects Codex subagent rows by toolName=collab_agent on the launch', () => {
    expect(
      isCodexSubagentTask(makeTrayTask({
        launch: makeItem({
          id: 'L',
          toolName: 'collab_agent',
          meta: JSON.stringify({ input: { tool: 'spawn_agent' } }),
        }),
      })),
    ).toBe(true);
  });

  it('does not classify orphan completions as launch-only Codex subagent tasks', () => {
    const completion = makeItem({
      id: 'C',
      kind: 'tool_completion',
      toolName: 'collab_agent',
      completionOf: 'L',
      status: 'completed',
    });

    expect(
      isCodexSubagentTask(makeTrayTask({
        launch: null,
        completion,
        anchor: completion,
      })),
    ).toBe(false);
  });

  it('returns false for unifiedExec rows (no toolName, or other Codex tools)', () => {
    expect(
      isCodexSubagentTask(makeTrayTask({
        launch: makeItem({ id: 'L', toolName: 'exec_command' }),
      })),
    ).toBe(false);
    expect(
      isCodexSubagentTask(makeTrayTask({
        launch: makeItem({ id: 'L', toolName: undefined }),
      })),
    ).toBe(false);
  });

  it('returns false for Claude rows (toolName=Bash, Task, etc.)', () => {
    expect(
      isCodexSubagentTask(makeTrayTask({
        launch: makeItem({ id: 'L', toolName: 'Bash' }),
      })),
    ).toBe(false);
    expect(
      isCodexSubagentTask(makeTrayTask({
        launch: makeItem({ id: 'L', toolName: 'Task' }),
      })),
    ).toBe(false);
  });
});

describe('isCodexStoppableTask', () => {
  it('returns true only for backgrounded Codex unifiedExec rows', () => {
    expect(
      isCodexStoppableTask(makeTrayTask({
        launch: makeItem({ id: 'bg', isBackground: true, toolName: 'exec_command' }),
      })),
    ).toBe(true);

    expect(
      isCodexStoppableTask(makeTrayTask({
        launch: makeItem({ id: 'pending', isBackground: false, toolName: 'exec_command' }),
      })),
    ).toBe(false);
  });

  it('returns false for Codex subagents even when they are background rows', () => {
    expect(
      isCodexStoppableTask(makeTrayTask({
        launch: makeItem({
          id: 'agent',
          isBackground: true,
          toolName: 'collab_agent',
          meta: JSON.stringify({ input: { tool: 'spawn_agent' } }),
        }),
      })),
    ).toBe(false);
  });
});

describe('trayRowStopTarget', () => {
  it('resolves the Claude task_id for a claude-task thread', () => {
    expect(
      trayRowStopTarget(
        makeTrayTask({
          launch: makeItem({ id: 'L', meta: JSON.stringify({ task_id: 'tsk-7' }) }),
        }),
        'claude-task',
      ),
    ).toBe('tsk-7');
  });

  it('resolves the Codex PTY process_id for a codex thread', () => {
    expect(
      trayRowStopTarget(
        makeTrayTask({
          launch: makeItem({
            id: 'L',
            isBackground: true,
            toolName: 'exec_command',
            meta: JSON.stringify({ source: 'unifiedExecStartup', process_id: '1734029' }),
          }),
        }),
        'codex-background-terminals',
      ),
    ).toBe('1734029');
  });

  it('never crosses the two id namespaces', () => {
    // A Claude row's task_id must not be offered to the Codex terminate
    // RPC (which parses the id as an i32 and would refuse it), and a
    // Codex process_id must not be offered to StopClaudeTask. Each
    // provider reads only its own key.
    const claudeRow = makeTrayTask({
      launch: makeItem({ id: 'L', meta: JSON.stringify({ task_id: 'tsk-7' }) }),
    });
    expect(trayRowStopTarget(claudeRow, 'codex-background-terminals')).toBeNull();

    const codexRow = makeTrayTask({
      launch: makeItem({
        id: 'L',
        isBackground: true,
        toolName: 'exec_command',
        meta: JSON.stringify({ source: 'unifiedExecStartup', process_id: '42' }),
      }),
    });
    expect(trayRowStopTarget(codexRow, 'claude-task')).toBeNull();
  });

  it('returns null for a Codex row the terminate RPC cannot reach', () => {
    // Not yielded yet: still a foreground command, not a background
    // terminal, so there is nothing for terminate to match.
    expect(
      trayRowStopTarget(
        makeTrayTask({
          launch: makeItem({
            id: 'pending',
            isBackground: false,
            toolName: 'exec_command',
            meta: JSON.stringify({ source: 'unifiedExecStartup', process_id: '42' }),
          }),
        }),
        'codex-background-terminals',
      ),
    ).toBeNull();

    // Spawned collab-agent child — a separate thread, and `close_agent`
    // is a model tool with no client path.
    expect(
      trayRowStopTarget(
        makeTrayTask({
          launch: makeItem({
            id: 'agent',
            isBackground: true,
            toolName: 'collab_agent',
            meta: JSON.stringify({ input: { tool: 'spawn_agent' }, process_id: '42' }),
          }),
        }),
        'codex-background-terminals',
      ),
    ).toBeNull();

    // Backgrounded, but the wire has not named a process id yet.
    expect(
      trayRowStopTarget(
        makeTrayTask({
          launch: makeItem({
            id: 'unnamed',
            isBackground: true,
            toolName: 'exec_command',
            meta: JSON.stringify({ source: 'unifiedExecStartup' }),
          }),
        }),
        'codex-background-terminals',
      ),
    ).toBeNull();
  });

  it('returns null for non-running rows and rows whose launch is gone', () => {
    const meta = JSON.stringify({
      task_id: 'tsk-7',
      source: 'unifiedExecStartup',
      process_id: '42',
    });
    for (const stop of ['claude-task', 'codex-background-terminals'] as const) {
      expect(
        trayRowStopTarget(
          makeTrayTask({
            status: 'completed',
            launch: makeItem({ id: 'L', isBackground: true, toolName: 'exec_command', meta }),
          }),
          stop,
        ),
      ).toBeNull();

      const completion = makeItem({ id: 'C', meta, completionOf: 'L' });
      expect(
        trayRowStopTarget(
          makeTrayTask({ launch: null, completion, anchor: completion, rowId: 'C' }),
          stop,
        ),
      ).toBeNull();
    }
  });

  it('returns null for a provider with no background stop primitive', () => {
    expect(
      trayRowStopTarget(
        makeTrayTask({
          launch: makeItem({
            id: 'L',
            isBackground: true,
            meta: JSON.stringify({ task_id: 'tsk-7', process_id: '42' }),
          }),
        }),
        'none',
      ),
    ).toBeNull();
  });
});

describe('statusLabel / statusClass', () => {
  it('labels active/error statuses in the compact tray style', () => {
    expect(statusLabel('running')).toBe('running');
    // Completed rows use Forge-style icon-only success chrome.
    expect(statusLabel('completed')).toBe('');
    expect(statusLabel('errored')).toBe('failed');
    expect(statusLabel('declined')).toBe('declined');
    // `killed` must NOT collapse into "Failed" — it's a user-initiated
    // stop; the label difference is the whole point of the phase.
    expect(statusLabel('killed')).toBe('stopped');
  });

  it('paints killed on muted-gray and keeps errored/declined on the error palette', () => {
    expect(statusClass('running')).toBe('text-accent');
    expect(statusClass('completed')).toContain('text-success');
    expect(statusClass('completed')).toContain('bg-success/10');
    expect(statusClass('errored')).toBe('text-error');
    expect(statusClass('declined')).toBe('text-error');
    // The phase spec calls for killed to read distinct from errored;
    // the class MUST NOT be the error palette.
    expect(statusClass('killed')).toBe('text-text-secondary');
    expect(statusClass('killed')).not.toBe('text-error');
  });
});

describe('formatElapsed', () => {
  it('formats sub-minute durations as whole seconds', () => {
    expect(formatElapsed(0)).toBe('0s');
    expect(formatElapsed(999)).toBe('0s');
    expect(formatElapsed(1_000)).toBe('1s');
    expect(formatElapsed(59_999)).toBe('59s');
  });

  it('formats minute-granularity durations as `Nm Ss`', () => {
    expect(formatElapsed(60_000)).toBe('1m 0s');
    expect(formatElapsed(125_000)).toBe('2m 5s');
    expect(formatElapsed(59 * 60_000 + 59_000)).toBe('59m 59s');
  });

  it('formats hour-granularity durations as `Nh Mm Ss`', () => {
    expect(formatElapsed(3_600_000)).toBe('1h 0m 0s');
    expect(formatElapsed(3_600_000 + 12 * 60_000)).toBe('1h 12m 0s');
  });
});

describe('trayTaskLabel', () => {
  it('prefers the launch summary over the completion summary', () => {
    const launch = makeItem({ id: 'L', summary: 'Bash: sleep 30' });
    const completion = makeItem({
      id: 'C',
      summary: 'Bash -> done',
      completionOf: 'L',
      status: 'completed',
    });
    const task: TrayTask = {
      rowId: 'L',
      anchor: launch,
      launch,
      completion,
      status: 'completed',
      elapsedMs: 0,
    };
    expect(trayTaskLabel(task)).toBe('Bash: sleep 30');
  });

  it('falls back to the completion summary when no launch is present', () => {
    const completion = makeItem({
      id: 'C',
      summary: 'Bash',
      completionOf: 'gone',
      status: 'completed',
    });
    const task: TrayTask = {
      rowId: 'C',
      anchor: completion,
      launch: null,
      completion,
      status: 'completed',
      elapsedMs: null,
    };
    expect(trayTaskLabel(task)).toBe('Bash');
  });

  it('falls back to "Tool" when neither side has a usable summary', () => {
    const launch = makeItem({ id: 'L', summary: '   ' });
    const task: TrayTask = {
      rowId: 'L',
      anchor: launch,
      launch,
      completion: null,
      status: 'running',
      elapsedMs: 0,
    };
    expect(trayTaskLabel(task)).toBe('Tool');
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

  it('maps a killed completion through to the killed status', () => {
    const launch = makeItem({ id: 'L', status: 'running', createdAt: 100, updatedAt: 100 });
    const completion = makeItem({
      id: 'C',
      status: 'killed',
      completionOf: 'L',
      createdAt: 500,
      updatedAt: 500,
      isBackground: false,
    });
    const out = deriveTrayTasks([launch, completion], 600, RETENTION_MS);
    expect(out).toHaveLength(1);
    expect(out[0].status).toBe('killed');
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

  it('sorts oldest created task first', () => {
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
    expect(out.map((t) => t.rowId)).toEqual(['stale', 'fresh']);
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

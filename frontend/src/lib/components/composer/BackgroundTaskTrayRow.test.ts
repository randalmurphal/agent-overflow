import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import BackgroundTaskTrayRow from './BackgroundTaskTrayRow.svelte';
import { makeItem } from '../../../test/helpers/chat';
import type { TrayTask } from '../../utils/backgroundTray';
import type { Item } from '../../types/models';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { ProviderID } from '../../providers/catalog';
import { tick } from 'svelte';
import { applySubagentProgress, resetForTest } from '../../stores/subagentProgress.svelte';
import { replaceSubagentRunStates, resetForTest as resetSubagentRunStates } from '../../stores/subagentRunState.svelte';

function taskFor(anchor: Item, overrides: Partial<TrayTask> = {}): TrayTask {
  return {
    rowId: anchor.id,
    anchor,
    launch: anchor,
    completion: null,
    status: 'running',
    elapsedMs: 3_000,
    depth: 0,
    ...overrides,
  };
}

function renderTrayRow(task: TrayTask, provider: ProviderID | null = 'codex') {
  return render(BackgroundTaskTrayRow, {
    props: {
      task,
      stopTarget: null,
      isStopping: false,
      provider,
      onStop: vi.fn(),
    },
  });
}

describe('<BackgroundTaskTrayRow>', () => {
  it('renders command tray rows with the backgrounded indicator', () => {
    const launch = makeItem({
      id: 'bg-command',
      kind: 'tool_call',
      toolName: 'exec_command',
      summary: 'sleep 30',
      status: 'running',
      isBackground: true,
      payloadKind: 'command_output',
      payloadMeta: JSON.stringify({ command: 'sleep 30', lineCount: 0, preview: '' }),
    });

    const { getByTestId } = renderTrayRow(taskFor(launch));

    expect(getByTestId('background-task-tray-row')).toHaveClass('bg-transparent');
    expect(getByTestId('background-task-tray-row')).not.toHaveClass('bg-surface-0');
    expect(getByTestId('command-output-label').textContent).toBe('bash');
    expect(getByTestId('command-output-status')).toHaveAttribute('data-state', 'backgrounded');
  });

  it('strips shell wrappers from live Codex tray command summaries', () => {
    const launch = makeItem({
      id: 'bg-codex-live-command',
      kind: 'tool_call',
      toolName: 'command_execution',
      summary: "Bash: /usr/bin/zsh -lc 'git status --short'",
      status: 'running',
      isBackground: true,
      meta: JSON.stringify({ source: 'unifiedExecStartup' }),
    });

    const { getByTestId, getByRole } = renderTrayRow(taskFor(launch));

    expect(getByTestId('command-output-command').textContent).toBe('git status --short');
    expect(getByRole('button', { name: 'Toggle Command Output: git status --short' })).toBeInTheDocument();
  });

  it('strips shell wrappers from truncated live Codex tray command summaries', () => {
    const fullCommand = "/usr/bin/zsh -lc 'uv run pytest tests/unit/db/test_migration_steps.py tests/unit/db/test_policy_steps.py'";
    const launch = makeItem({
      id: 'bg-codex-truncated-command',
      kind: 'tool_call',
      toolName: 'command_execution',
      summary: "Bash: /usr/bin/zsh -lc 'uv run pytest tests/unit/db/test_migration_steps.py tests/uni…",
      status: 'running',
      isBackground: true,
      meta: JSON.stringify({ source: 'unifiedExecStartup', command: fullCommand }),
    });

    const { getByTestId, queryByText } = renderTrayRow(taskFor(launch));

    expect(getByTestId('command-output-command').textContent).toBe(
      'uv run pytest tests/unit/db/test_migration_steps.py tests/unit/db/test_policy_steps.py',
    );
    expect(getByTestId('command-output-command')).toHaveAttribute(
      'title',
      'uv run pytest tests/unit/db/test_migration_steps.py tests/unit/db/test_policy_steps.py',
    );
    expect(queryByText("/usr/bin/zsh -lc 'uv run pytest tests/unit/db/test_migration_steps.py tests/uni…")).toBeNull();
  });

  it('strips shell wrappers from completed Codex tray command payload metadata', () => {
    const launch = makeItem({
      id: 'bg-codex-launch',
      kind: 'tool_call',
      toolName: 'command_execution',
      summary: "Bash: /usr/bin/zsh -lc 'pnpm test'",
      status: 'running',
      isBackground: true,
      meta: JSON.stringify({ source: 'unifiedExecStartup' }),
    });
    const completion = makeItem({
      id: 'bg-codex-completion',
      kind: 'tool_completion',
      completionOf: launch.id,
      toolName: 'command_execution',
      status: 'completed',
      payloadKind: 'command_output',
      payloadId: 'payload-command',
      payloadMeta: JSON.stringify({
        command: "/usr/bin/zsh -lc 'pnpm test'",
        exitCode: 0,
        lineCount: 4,
        preview: 'pass',
      }),
    });

    const { getByTestId, queryByText } = renderTrayRow(taskFor(launch, {
      completion,
      status: 'completed',
      elapsedMs: 8_000,
    }));

    expect(getByTestId('command-output-command').textContent).toBe('pnpm test');
    expect(queryByText("/usr/bin/zsh -lc 'pnpm test'")).toBeNull();
  });

  it('routes Claude Agent tray rows through AgentRow', () => {
    const launch = makeItem({
      id: 'bg-agent',
      kind: 'tool_call',
      toolName: 'Agent',
      summary: 'Agent: inspect tests',
      status: 'running',
      isBackground: true,
      payloadMeta: JSON.stringify({
        input: { subagent_type: 'Explorer', description: 'Inspect tests' },
      }),
    });

    const { getByTestId } = renderTrayRow(taskFor(launch), 'claude');

    expect(getByTestId('agent-row-label').textContent).toBe('agent');
    expect(getByTestId('agent-row-preview').textContent).toContain('Explorer');
  });

  it('routes Codex collab tray rows through CollabToolRow with agent status enabled', () => {
    const launch = makeItem({
      id: 'bg-collab',
      kind: 'tool_call',
      toolName: 'collab_agent',
      summary: 'spawn agent',
      status: 'running',
      meta: JSON.stringify({
        input: {
          tool: 'spawn_agent',
          receiverThreadIds: ['agent-1'],
          prompt: 'Inspect renderer coverage',
        },
      }),
    });

    const { getByTestId } = renderTrayRow(taskFor(launch));

    expect(getByTestId('collab-tool-row-label').textContent).toBe('agent');
    expect(getByTestId('collab-tool-row-status-slot').querySelector('[data-state="running"]')).not.toBeNull();
  });

  it('shows the latest projected Codex child tool beneath the live tray row', () => {
    const launch = makeItem({
      id: 'bg-collab-tool',
      threadId: 'thread-1',
      kind: 'tool_call',
      toolName: 'collab_agent',
      summary: 'spawn agent',
      status: 'running',
      meta: JSON.stringify({
        input: {
          tool: 'spawn_agent',
          receiverThreadIds: ['agent-1'],
        },
        subagentLatestToolSummary: 'Bash: pnpm test',
      }),
    });

    const { getByTestId } = renderTrayRow(taskFor(launch));

    expect(getByTestId('background-task-tray-row-activity').textContent).toContain('Bash: pnpm test');
  });

  it('shows a Claude agent row\u2019s latest-tool decoration when no live activity is ticking', () => {
    const launch = makeItem({
      id: 'bg-claude-agent',
      kind: 'tool_call',
      toolName: 'Agent',
      status: 'running',
      isBackground: true,
      payloadMeta: JSON.stringify({ input: { subagent_type: 'Explorer', description: 'dig' } }),
      meta: JSON.stringify({ subagentLatestToolSummary: 'Read: parser.go' }),
    });

    const { getByTestId } = renderTrayRow(taskFor(launch), 'claude');

    expect(getByTestId('background-task-tray-row-activity').textContent).toContain('Read: parser.go');
  });

  it('routes generic tray rows through GenericToolCallRow', () => {
    const launch = makeItem({
      id: 'bg-generic',
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'README.md',
      status: 'running',
    });

    const { getByTestId } = renderTrayRow(taskFor(launch), 'claude');

    expect(getByTestId('tool-call-card-label').textContent).toBe('read');
  });
});

describe('<BackgroundTaskTrayRow> doors (agent-visibility)', () => {
  const agentAnchor = () =>
    makeItem({
      id: 'L1',
      kind: 'tool_call',
      toolName: 'Agent',
      status: 'running',
      payloadMeta: JSON.stringify({ input: { subagent_type: 'Explorer', description: 'dig' } }),
    });
  // The digest needs a real pane; a stub with a pane id is enough for the
  // header's disclosure state and the collapsed row never mounts it.
  const paneStub = { paneId: 'main' } as unknown as ThreadPane;

  it('opens the pane from the open button, toggles the digest from the header, neither from Stop', async () => {
    const onOpenPane = vi.fn();
    const onToggleExpanded = vi.fn();
    const onStop = vi.fn();
    const { getByTestId } = render(BackgroundTaskTrayRow, {
      props: {
        task: taskFor(agentAnchor(), { depth: 2 }),
        stopTarget: 'task-1',
        isStopping: false,
        provider: 'claude' as ProviderID,
        onStop,
        onOpenPane,
        pane: paneStub,
        expanded: false,
        onToggleExpanded,
      },
    });
    const row = getByTestId('background-task-tray-row');
    expect(row.getAttribute('data-depth')).toBe('2');
    expect(row.getAttribute('style')).toContain('margin-left');
    expect(row.className).not.toContain('cursor-pointer');

    const header = getByTestId('agent-row-toggle');
    expect(header).toHaveAttribute('aria-expanded', 'false');
    await fireEvent.click(header);
    expect(onToggleExpanded).toHaveBeenCalledTimes(1);
    expect(onOpenPane).not.toHaveBeenCalled();
    await fireEvent.click(getByTestId('background-task-tray-row-open'));
    expect(onOpenPane).toHaveBeenCalledTimes(1);
    expect(onToggleExpanded).toHaveBeenCalledTimes(1);
    await fireEvent.click(getByTestId('background-task-tray-row-stop'));
    expect(onStop).toHaveBeenCalledTimes(1);
    expect(onToggleExpanded).toHaveBeenCalledTimes(1);
    expect(onOpenPane).toHaveBeenCalledTimes(1);
  });

  it('keeps the open button at every width', () => {
    const { getByTestId } = render(BackgroundTaskTrayRow, {
      props: {
        task: taskFor(agentAnchor()),
        stopTarget: null,
        isStopping: false,
        provider: 'claude' as ProviderID,
        onStop: vi.fn(),
        onOpenPane: vi.fn(),
      },
    });
    expect(getByTestId('background-task-tray-row-open').className).not.toContain('hidden');
  });

  it('falls back to opening the pane from the header when no pane can host a digest', async () => {
    const onOpenPane = vi.fn();
    const { getByTestId } = render(BackgroundTaskTrayRow, {
      props: {
        task: taskFor(agentAnchor()),
        stopTarget: null,
        isStopping: false,
        provider: 'claude' as ProviderID,
        onStop: vi.fn(),
        onOpenPane,
      },
    });
    const header = getByTestId('agent-row-toggle');
    expect(header).not.toHaveAttribute('aria-expanded', 'true');
    await fireEvent.click(header);
    expect(onOpenPane).toHaveBeenCalledTimes(1);
  });

  it('gives a plain command row no open button and a live chevron only once output exists', async () => {
    const onOpenPane = vi.fn();
    const onToggleExpanded = vi.fn();
    const anchor = makeItem({
      id: 'L1', kind: 'tool_call', toolName: 'Bash', status: 'running', summary: 'sleep 30',
      meta: JSON.stringify({ input: { command: 'sleep 30', description: 'Wait thirty seconds' } }),
    });
    const view = render(BackgroundTaskTrayRow, {
      props: {
        task: taskFor(anchor),
        stopTarget: 'task-1',
        isStopping: false,
        provider: 'claude' as ProviderID,
        onStop: vi.fn(),
        // Supplied, so the button's absence below is the Bash gate and not
        // just a missing handler.
        onOpenPane,
        pane: paneStub,
        onToggleExpanded,
      },
    });
    expect(view.queryByTestId('background-task-tray-row-open')).toBeNull();
    expect(view.getByTestId('command-output-command')).toHaveTextContent('Wait thirty seconds');
    expect(view.getByTestId('command-output-command')).toHaveAttribute('title', 'sleep 30');
    const header = view.getByTestId('command-output-toggle');
    expect(header).toHaveAttribute('aria-disabled', 'true');
    await fireEvent.click(header);
    expect(onOpenPane).not.toHaveBeenCalled();
    expect(onToggleExpanded).not.toHaveBeenCalled();
    expect(view.queryByTestId('command-output-full-command')).toBeNull();

    const completion = makeItem({
      id: 'C1',
      kind: 'tool_completion',
      toolName: 'Bash',
      status: 'completed',
      completionOf: 'L1',
      payloadId: 'payload-1',
      payloadKind: 'command_output',
      payloadMeta: JSON.stringify({ command: 'sleep 30', lineCount: 1, preview: 'done' }),
    });
    await view.rerender({ task: taskFor(anchor, { completion, status: 'completed' }), stopTarget: null });
    expect(view.getByTestId('command-output-command')).toHaveTextContent('Wait thirty seconds');
    expect(view.getByTestId('command-output-toggle')).not.toHaveAttribute('aria-disabled', 'true');
    expect(view.queryByTestId('background-task-tray-row-digest')).toBeNull();
  });

  it('shows a running agent\u2019s live tool count, tokens and activity line', () => {
    // The tray is a running background agent's live surface (user ruling
    // 2026-08-23): the launch row never changes and the card does not
    // exist until the completion lands, so the counters show here.
    const launch = makeItem({
      id: 'tu-agent',
      threadId: 'thread-1',
      kind: 'tool_call',
      toolName: 'Agent',
      status: 'running',
      isBackground: true,
      payloadMeta: JSON.stringify({
        toolName: 'Agent',
        input: { description: 'scan the parser', subagent_type: 'scanner' },
      }),
    });
    applySubagentProgress({
      threadId: 'thread-1',
      itemId: 'tu-agent',
      progress: { toolUses: 3, totalTokens: 18_227, activity: 'Scanning the parser for drift' },
      updatedAt: 1,
    });
    try {
      const { getByTestId } = renderTrayRow(taskFor(launch), 'claude');
      expect(getByTestId('background-task-tray-row-tools').textContent?.trim()).toBe('3 tools');
      expect(getByTestId('background-task-tray-row-tokens').textContent?.trim()).toBe('18.2k tokens');
      expect(getByTestId('background-task-tray-row-activity').textContent).toContain(
        'Scanning the parser for drift',
      );
    } finally {
      resetForTest();
    }
  });
});

describe('<BackgroundTaskTrayRow> agent run state', () => {
  const agentLaunch = () => makeItem({
    id: 'bg-agent',
    threadId: 'thread-1',
    kind: 'tool_call',
    toolName: 'Agent',
    summary: 'Agent: inspect tests',
    status: 'running',
    isBackground: true,
    payloadMeta: JSON.stringify({
      toolName: 'Agent',
      input: { subagent_type: 'Explorer', description: 'Inspect tests' },
    }),
  });

  afterEach(() => {
    resetForTest();
    resetSubagentRunStates();
  });

  it('shows a parked agent as parked: hollow indicator, the waiting line over the live activity, and the report head', () => {
    applySubagentProgress({
      threadId: 'thread-1',
      itemId: 'bg-agent',
      progress: { toolUses: 3, totalTokens: 1_200, activity: 'Reading fork_moves.go' },
      updatedAt: 1,
    });
    replaceSubagentRunStates('thread-1', new Map([[
      'bg-agent',
      { state: 'parked', waitingOn: 2, report: { id: 'report-1', preview: 'Found the race in fork_moves.go.' } },
    ]]));
    const { getByTestId } = renderTrayRow(taskFor(agentLaunch()), 'claude');
    expect(getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'parked');
    const indicator = getByTestId('agent-row-status').querySelector('[data-testid="indicator"]');
    expect(indicator?.getAttribute('data-state')).toBe('parked');
    expect(indicator?.getAttribute('aria-label')).toBe('Parked');
    expect(getByTestId('background-task-tray-row-activity').textContent).toContain('Waiting on 2 background commands');
    expect(getByTestId('background-task-tray-row-activity').textContent).not.toContain('Reading fork_moves.go');
    expect(getByTestId('background-task-tray-row-report').textContent).toContain('Found the race in fork_moves.go.');
    expect(getByTestId('background-task-tray-row-tools').textContent?.trim()).toBe('3 tools');
  });

  it('words a single parked command in the singular and omits the report line when the agent wrote none', () => {
    replaceSubagentRunStates('thread-1', new Map([[
      'bg-agent',
      { state: 'parked', waitingOn: 1, report: null },
    ]]));
    const { getByTestId, queryByTestId } = renderTrayRow(taskFor(agentLaunch()), 'claude');
    expect(getByTestId('background-task-tray-row-activity').textContent).toContain('Waiting on 1 background command');
    expect(getByTestId('background-task-tray-row-activity').textContent).not.toContain('commands');
    expect(queryByTestId('background-task-tray-row-report')).toBeNull();
  });

  it('keeps a running agent on the backgrounded indicator and its live activity', () => {
    applySubagentProgress({
      threadId: 'thread-1',
      itemId: 'bg-agent',
      progress: { toolUses: 1, totalTokens: 100, activity: 'Reading fork_moves.go' },
      updatedAt: 1,
    });
    replaceSubagentRunStates('thread-1', new Map([['bg-agent', { state: 'running', waitingOn: 0, report: null }]]));
    const { getByTestId, queryByTestId } = renderTrayRow(taskFor(agentLaunch()), 'claude');
    expect(getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'running');
    expect(getByTestId('agent-row-status').querySelector('[data-testid="indicator"]')?.getAttribute('data-state')).toBe('backgrounded');
    expect(getByTestId('background-task-tray-row-activity').textContent).toContain('Reading fork_moves.go');
    expect(queryByTestId('background-task-tray-row-report')).toBeNull();
  });

  it('drops the parked presentation once the served state flips back to running', async () => {
    replaceSubagentRunStates('thread-1', new Map([[
      'bg-agent',
      { state: 'parked', waitingOn: 1, report: { id: 'report-1', preview: 'Found it.' } },
    ]]));
    const view = renderTrayRow(taskFor(agentLaunch()), 'claude');
    expect(view.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'parked');
    replaceSubagentRunStates('thread-1', new Map([['bg-agent', { state: 'running', waitingOn: 0, report: null }]]));
    await tick();
    expect(view.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'running');
    expect(view.getByTestId('agent-row-status').querySelector('[data-testid="indicator"]')?.getAttribute('data-state')).toBe('backgrounded');
    expect(view.queryByTestId('background-task-tray-row-report')).toBeNull();
    // No live activity and no parked line: the activity slot is gone.
    expect(view.queryByTestId('background-task-tray-row-activity')).toBeNull();
  });

  it('says the session ended when the completion sibling was written by session death, not "stopped"', () => {
    const launch = agentLaunch();
    const sessionDied = makeItem({
      id: 'complete:bg-agent',
      threadId: 'thread-1',
      kind: 'tool_completion',
      toolName: 'Agent',
      status: 'killed',
      completionOf: launch.id,
      meta: JSON.stringify({ task_id: 'task-agent', status_source: 'session_died' }),
    });
    const died = renderTrayRow(taskFor(launch, { completion: sessionDied, status: 'completed' }), 'claude');
    expect(died.getByTestId('agent-row-error').textContent).toContain('Session ended before the agent finished');
    expect(died.getByTestId('background-task-tray-row-status')).not.toHaveAttribute('data-run-state');
    died.unmount();

    const stopped = renderTrayRow(taskFor(launch, {
      completion: { ...sessionDied, meta: JSON.stringify({ task_id: 'task-agent', status_source: 'host_exit' }) },
      status: 'completed',
    }), 'claude');
    expect(stopped.getByTestId('agent-row-error').textContent).toContain('Tool call stopped');
  });
});

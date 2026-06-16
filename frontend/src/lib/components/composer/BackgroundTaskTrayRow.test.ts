import { describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import BackgroundTaskTrayRow from './BackgroundTaskTrayRow.svelte';
import { makeItem } from '../../../test/helpers/chat';
import type { TrayTask } from '../../utils/backgroundTray';
import type { Item } from '../../types/models';
import type { ProviderID } from '../../providers/catalog';

function taskFor(anchor: Item, overrides: Partial<TrayTask> = {}): TrayTask {
  return {
    rowId: anchor.id,
    anchor,
    launch: anchor,
    completion: null,
    status: 'running',
    elapsedMs: 3_000,
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

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

  it('routes Codex collab tray rows through CollabToolRow with spawn status enabled', () => {
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

    expect(getByTestId('collab-tool-row-label').textContent).toBe('spawn');
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
